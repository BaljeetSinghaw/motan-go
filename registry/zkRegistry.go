package registry

import (
	"encoding/binary"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/weibocom/motan-go/cluster"
	motan "github.com/weibocom/motan-go/core"
	"github.com/weibocom/motan-go/log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zkRegistryNamespace         = "/motan"
	zkRegistryCommand           = "/command"
	zkRegistryNode              = "/node"
	zkPathSeparator             = "/"
	zkNodeTypeServer            = "server"
	zkNodeTypeUnavailableServer = "unavailableServer"
	zkNodeTypeClient            = "client"
	zkNodeTypeAgent             = "agent"
	zKDefaultSessionTimeout     = 1000

	//Compatible with java ioStream
	streamMagicTag = 0xaced
	shortStringTag = 0x74
	longStringTag  = 0x7C
)

type ZkRegistry struct {
	available            bool
	zkConn               *zk.Conn
	url                  *motan.URL
	sessionTimeout       time.Duration
	registerLock         sync.Mutex
	subscribeLock        sync.Mutex
	switcherMap          map[string]chan bool
	registeredServiceMap map[string]*motan.URL
	availableServiceMap  map[string]*motan.URL
	subscribedServiceMap map[string]map[motan.NotifyListener]*motan.URL
	subscribedCommandMap map[string]map[motan.CommandNotifyListener]*motan.URL
}

func (z *ZkRegistry) Initialize() {
	z.sessionTimeout = time.Duration(
		z.url.GetPositiveIntValue(motan.SessionTimeOutKey, zKDefaultSessionTimeout)) * time.Second
	z.subscribedServiceMap = make(map[string]map[motan.NotifyListener]*motan.URL)
	z.subscribedCommandMap = make(map[string]map[motan.CommandNotifyListener]*motan.URL)
	z.switcherMap = make(map[string]chan bool)
	z.registeredServiceMap = make(map[string]*motan.URL)
	z.availableServiceMap = make(map[string]*motan.URL)
	addrs := motan.TrimSplit(z.url.GetAddressStr(), ",")
	c, ch, err := zk.Connect(addrs, z.sessionTimeout)
	if err != nil {
		vlog.Errorf("[ZkRegistry] connect server error. err:%v\n", err)
		return
	}
	z.zkConn = c
	go z.handleNewSession(ch)
	z.setAvailable(true)
}

func (z *ZkRegistry) handleNewSession(ch <-chan zk.Event) {
	defer motan.HandlePanic(nil)
	for {
		ev := <-ch
		if ev.State == zk.StateDisconnected {
			z.setAvailable(false)
		} else if ev.State == zk.StateHasSession && !z.IsAvailable() {
			z.setAvailable(true)
			vlog.Infoln("[ZkRegistry] get new session notify")
			z.recoverService()
			z.recoverSubscribe()
		}
	}
}

func (z *ZkRegistry) recoverService() {
	z.registerLock.Lock()
	defer z.registerLock.Unlock()
	if len(z.registeredServiceMap) > 0 {
		for _, url := range z.registeredServiceMap {
			z.doRegister(url)
		}
		vlog.Infoln("[ZkRegistry] register services success:", z.registeredServiceMap)
	}
	if len(z.availableServiceMap) > 0 {
		for _, url := range z.availableServiceMap {
			z.doAvailable(url)
		}
		vlog.Infoln("[ZkRegistry] available services success:", z.availableServiceMap)
	}
}

func (z *ZkRegistry) recoverSubscribe() {
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	if len(z.subscribedServiceMap) > 0 {
		for _, listeners := range z.subscribedServiceMap {
			for _, url := range listeners {
				z.doSubscribe(url)
			}
		}
		vlog.Infoln("[ZkRegistry] subscribe services success")
	}
	if len(z.subscribedCommandMap) > 0 {
		for _, listeners := range z.subscribedCommandMap {
			for _, url := range listeners {
				z.doSubscribeCommand(url)
			}
		}
		vlog.Infoln("[ZkRegistry] subscribe commands success")
	}
}

func (z *ZkRegistry) Register(url *motan.URL) {
	if !z.IsAvailable() {
		return
	}
	z.registerLock.Lock()
	defer z.registerLock.Unlock()
	if _, ok := z.registeredServiceMap[url.GetIdentity()]; !ok {
		vlog.Infof("[ZkRegistry] register service. url:%s\n", url.GetIdentity())
		z.doRegister(url)
		z.registeredServiceMap[url.GetIdentity()] = url
	}
}

func (z *ZkRegistry) doRegister(url *motan.URL) {
	if url.Group == "" || url.Path == "" || url.Host == "" {
		vlog.Errorf("[ZkRegistry] register service fail. invalid url:%s\n", url.GetIdentity())
	}
	if IsAgent(url) {
		z.createNode(url, zkNodeTypeAgent)
	} else {
		z.removeNode(url, zkNodeTypeServer)
		z.createNode(url, zkNodeTypeUnavailableServer)
	}
}

func (z *ZkRegistry) UnRegister(url *motan.URL) {
	if !z.IsAvailable() {
		return
	}
	z.registerLock.Lock()
	defer z.registerLock.Unlock()
	if _, ok := z.registeredServiceMap[url.GetIdentity()]; ok {
		vlog.Infof("[ZkRegistry] unregister service. url:%s\n", url.GetIdentity())
		z.removeNode(url, zkNodeTypeServer)
		z.removeNode(url, zkNodeTypeUnavailableServer)
		delete(z.registeredServiceMap, url.GetIdentity())
	}
}

func (z *ZkRegistry) Subscribe(url *motan.URL, listener motan.NotifyListener) {
	if !z.IsAvailable() {
		return
	}
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	servicePath := toNodeTypePath(url, zkNodeTypeServer)
	if listeners, ok := z.subscribedServiceMap[servicePath]; ok {
		listeners[listener] = url
		vlog.Infof("[ZkRegistry] subscribe service success. path:%s, listener:%s\n", servicePath, listener.GetIdentity())
		return
	}
	lisMap := make(map[motan.NotifyListener]*motan.URL)
	lisMap[listener] = url
	z.subscribedServiceMap[servicePath] = lisMap
	vlog.Infof("[ZkRegistry] subscribe service. url:%s\n", url.GetIdentity())
	z.doSubscribe(url)
}

func (z *ZkRegistry) doSubscribe(url *motan.URL) {
	servicePath := toNodeTypePath(url, zkNodeTypeServer)
	if isExist, _, err := z.zkConn.Exists(servicePath); err != nil || !isExist {
		vlog.Errorf("[ZkRegistry] check service exists fail. isExist:%v, path:%s, err:%v, \n", isExist, servicePath, err)
		return
	}
	_, _, ch, err := z.zkConn.ChildrenW(servicePath)
	if err != nil {
		vlog.Errorf("[ZkRegistry] subscribe service error. err:%v\n", err)
		return
	}
	switcherChan, ok := z.switcherMap[servicePath]
	if !ok {
		switcherChan = make(chan bool)
		z.switcherMap[servicePath] = switcherChan
	}
	vlog.Infof("[ZkRegistry] start watch server node. path:%s\n", servicePath)
	url.PutParam(motan.NodeTypeKey, motan.NodeTypeReferer) // all subscribe url must as referer
	if url.Host == "" {
		url.Host = motan.GetLocalIP()
	}
	z.createNode(url, zkNodeTypeClient) // register as rpc client
	go func() {
		defer motan.HandlePanic(nil)
		for {
			select {
			case evt := <-ch:
				if evt.Type == zk.EventNodeChildrenChanged {
					if nodes, _, chx, err := z.zkConn.ChildrenW(servicePath); err == nil {
						z.saveSnapshot(nodes, url)
						ch = chx
						listeners, ok := z.subscribedServiceMap[servicePath]
						if ok && len(nodes) > 0 {
							for lis := range listeners {
								lis.Notify(z.url, z.nodeChildsToURLs(url, servicePath, nodes))
								vlog.Infof("[ZkRegistry] notify nodes:%+v\n", nodes)
							}
						}
					} else {
						vlog.Errorln("[ZkRegistry] watch server node error. err:", err)
					}
				} else if evt.Type == zk.EventNotWatching {
					vlog.Infoln("[ZkRegistry] not watch server node. path:", servicePath)
					return
				}
			case checkWatch := <-switcherChan:
				if !checkWatch {
					close(switcherChan)
					delete(z.switcherMap, servicePath)
					return
				}
			}
		}
	}()
}

func (z *ZkRegistry) Unsubscribe(url *motan.URL, listener motan.NotifyListener) {
	if !z.IsAvailable() {
		return
	}
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	servicePath := toNodeTypePath(url, zkNodeTypeServer)
	if _, ok := z.subscribedServiceMap[servicePath]; ok {
		vlog.Infof("[ZkRegistry] unsubscribe service. url:%s\n", url.GetIdentity())
		delete(z.subscribedServiceMap[servicePath], listener)
		if switcherChan, ok := z.switcherMap[servicePath]; ok && len(z.subscribedServiceMap[servicePath]) < 1 {
			switcherChan <- false
			delete(z.subscribedServiceMap, servicePath)
		}
	}
}

func (z *ZkRegistry) Discover(url *motan.URL) []*motan.URL {
	if !z.IsAvailable() {
		return nil
	}
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	nodePath := toNodeTypePath(url, zkNodeTypeServer)
	nodes, _, err := z.zkConn.Children(nodePath)
	if err == nil {
		z.saveSnapshot(nodes, url)
		return z.nodeChildsToURLs(url, nodePath, nodes)
	}
	vlog.Errorf("[ZkRegistry] discover service error! url:%s, err:%v\n", url.GetIdentity(), err)
	return nil
}

func (z *ZkRegistry) SubscribeCommand(url *motan.URL, listener motan.CommandNotifyListener) {
	if !z.IsAvailable() {
		return
	}
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	commandPath := ""
	if IsAgent(url) {
		commandPath = toAgentCommandPath(url)
	} else {
		commandPath = toCommandPath(url)
	}
	if listeners, ok := z.subscribedCommandMap[commandPath]; ok && listeners != nil {
		vlog.Infof("[ZkRegistry] subscribe command success. path:%s, listener:%s\n", commandPath, listener.GetIdentity())
		listeners[listener] = url
		return
	}
	lisMap := make(map[motan.CommandNotifyListener]*motan.URL)
	lisMap[listener] = url
	z.subscribedCommandMap[commandPath] = lisMap
	vlog.Infof("[ZkRegistry] subscribe command success. path:%s, url:%s\n", commandPath, url.GetIdentity())
	z.doSubscribeCommand(url)
}

func (z *ZkRegistry) doSubscribeCommand(url *motan.URL) {
	var commandPath string
	if IsAgent(url) {
		commandPath = toAgentCommandPath(url)
	} else {
		commandPath = toCommandPath(url)
	}
	isExist, _, err := z.zkConn.Exists(commandPath)
	if err != nil {
		vlog.Errorf("[ZkRegistry] check command exists err. path:%s, err:%v, \n", commandPath, err)
		return
	}
	if !isExist {
		z.createPersistent(commandPath, false)
	}
	_, _, ch, err := z.zkConn.GetW(commandPath)
	if err != nil {
		vlog.Errorf("[ZkRegistry] subscribe command error. commandPath:%s, url:%v, err:%v\n", commandPath, url, err)
		return
	}
	switcherChan, ok := z.switcherMap[commandPath]
	if !ok {
		switcherChan = make(chan bool)
		z.switcherMap[commandPath] = switcherChan
	}
	vlog.Infof("[ZkRegistry] start watch command %s\n", commandPath)
	go func() {
		defer motan.HandlePanic(nil)
		for {
			select {
			case evt := <-ch:
				if evt.Type == zk.EventNodeDataChanged {
					if data, _, chx, err := z.zkConn.GetW(commandPath); err == nil {
						ch = chx
						if listeners, ok := z.subscribedCommandMap[commandPath]; ok && len(data) > 0 {
							cmdInfo := getNodeInfo(data)
							for lis := range listeners {
								lis.NotifyCommand(url, cluster.ServiceCmd, cmdInfo)
								vlog.Infof("[ZkRegistry] command changed, path:%s, cmdInfo:%s\n", commandPath, cmdInfo)
							}
						}
					} else {
						vlog.Errorf("[ZkRegistry] command changed, get cmdInfo error, err:%v\n", err)
					}
				} else if evt.Type == zk.EventNotWatching {
					vlog.Infoln("[ZkRegistry] not watching commandPath:", commandPath)
					return
				}
			case checkWatch := <-switcherChan:
				if !checkWatch {
					close(switcherChan)
					delete(z.switcherMap, commandPath)
					return
				}
			}
		}
	}()
}

func (z *ZkRegistry) UnSubscribeCommand(url *motan.URL, listener motan.CommandNotifyListener) {
	if !z.IsAvailable() {
		return
	}
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	var commandPath string
	if IsAgent(url) {
		commandPath = toAgentCommandPath(url)
	} else {
		commandPath = toCommandPath(url)
	}
	if _, ok := z.subscribedCommandMap[commandPath]; ok {
		vlog.Infof("[ZkRegistry] unsubscribe command. url:%s\n", url.GetIdentity())
		delete(z.subscribedCommandMap[commandPath], listener)
		if switcherChan, ok := z.switcherMap[commandPath]; ok && len(z.subscribedCommandMap[commandPath]) < 1 {
			switcherChan <- false
			delete(z.subscribedCommandMap, commandPath)
		}
	}
}

func (z *ZkRegistry) DiscoverCommand(url *motan.URL) string {
	if !z.IsAvailable() {
		return ""
	}
	var res string
	var commandPath string
	if IsAgent(url) {
		commandPath = toAgentCommandPath(url)
	} else {
		commandPath = toCommandPath(url)
	}
	isExist, _, err := z.zkConn.Exists(commandPath)
	if err != nil || !isExist {
		vlog.Errorf("[ZkRegistry] check command exists err. path:%s, isExit:%v, err:%v, \n", commandPath, isExist, err)
		return res
	}
	if data, _, err := z.zkConn.Get(commandPath); err == nil {
		vlog.Infof("[ZkRegistry] discover command. path:%s\n", commandPath)
		res = getNodeInfo(data)
	} else {
		vlog.Errorf("[ZkRegistry] discover command error. url:%s, err:%s\n", url.GetIdentity(), err.Error())
	}
	return res
}

func (z *ZkRegistry) Available(url *motan.URL) {
	if !z.IsAvailable() {
		return
	}
	z.registerLock.Lock()
	z.registerLock.Unlock()
	if url == nil {
		vlog.Infof("[ZkRegistry] available all services:%v\n", z.registeredServiceMap)
	} else {
		vlog.Infof("[ZkRegistry] available service:%s\n", url.GetIdentity())
	}
	z.doAvailable(url)
}

func (z *ZkRegistry) doAvailable(url *motan.URL) {
	if url == nil {
		for _, u := range z.registeredServiceMap {
			z.removeNode(u, zkNodeTypeUnavailableServer)
			z.createNode(u, zkNodeTypeServer)
			z.availableServiceMap[u.GetIdentity()] = url
		}
	} else {
		z.removeNode(url, zkNodeTypeUnavailableServer)
		z.createNode(url, zkNodeTypeServer)
		z.availableServiceMap[url.GetIdentity()] = url
	}
}

func (z *ZkRegistry) Unavailable(url *motan.URL) {
	if !z.IsAvailable() {
		return
	}
	z.registerLock.Lock()
	z.registerLock.Unlock()
	if url == nil {
		vlog.Infof("[ZkRegistry] unavailable all services:%v\n", z.registeredServiceMap)
	} else {
		vlog.Infof("[ZkRegistry] unavailable service. url:%s\n", url.GetIdentity())
	}
	z.doUnavailable(url)
}

func (z *ZkRegistry) doUnavailable(url *motan.URL) {
	if url == nil {
		for _, u := range z.registeredServiceMap {
			z.removeNode(u, zkNodeTypeServer)
			z.createNode(u, zkNodeTypeUnavailableServer)
			delete(z.availableServiceMap, u.GetIdentity())
		}
	} else {
		z.removeNode(url, zkNodeTypeServer)
		z.createNode(url, zkNodeTypeUnavailableServer)
		delete(z.availableServiceMap, url.GetIdentity())
	}
}

func (z *ZkRegistry) GetRegisteredServices() []*motan.URL {
	z.registerLock.Lock()
	defer z.registerLock.Unlock()
	urls := make([]*motan.URL, 0, len(z.registeredServiceMap))
	for _, u := range z.registeredServiceMap {
		urls = append(urls, u)
	}
	return urls
}

func (z *ZkRegistry) GetURL() *motan.URL {
	return z.url
}

func (z *ZkRegistry) SetURL(url *motan.URL) {
	z.url = url
}

func (z *ZkRegistry) GetName() string {
	return "zookeeper"
}

func (z *ZkRegistry) IsAvailable() bool {
	return z.available
}

func (z *ZkRegistry) setAvailable(available bool) {
	z.available = available
}

func (z *ZkRegistry) StartSnapshot(conf *motan.SnapshotConf) {}

func (z *ZkRegistry) saveSnapshot(nodes []string, url *motan.URL) {
	serviceNode := ServiceNode{
		Group: url.Group,
		Path:  url.Path,
	}
	nodeInfos := make([]SnapshotNodeInfo, 0, len(nodes))
	for _, addr := range nodes {
		nodeInfos = append(nodeInfos, SnapshotNodeInfo{Addr: addr})
	}
	serviceNode.Nodes = nodeInfos
	SaveSnapshot(z.GetURL().GetIdentity(), GetNodeKey(url), serviceNode)
}

func (z *ZkRegistry) removeNode(url *motan.URL, nodeType string) {
	var nodePath string
	if nodeType == zkNodeTypeAgent {
		nodePath = toAgentNodePath(url)
	} else {
		nodePath = toNodePath(url, nodeType)
	}
	isExist, stats, err := z.zkConn.Exists(nodePath)
	if err == nil && isExist {
		if err = z.zkConn.Delete(nodePath, stats.Version); err == nil {
			return
		}
	}
	if err != nil {
		vlog.Errorf("[ZkRegistry] remove node error. err:%v, isExist:%v\n", err, isExist)
	}
}

func (z *ZkRegistry) createNode(url *motan.URL, nodeType string) {
	var typePath string
	var nodePath string
	if nodeType == zkNodeTypeAgent {
		typePath = toAgentNodeTypePath(url)
		nodePath = toAgentNodePath(url)
	} else {
		typePath = toNodeTypePath(url, nodeType)
		nodePath = toNodePath(url, nodeType)
	}
	z.removeNode(url, nodeType)
	if isExist, _, err := z.zkConn.Exists(typePath); err != nil {
		vlog.Errorf("[ZkRegistry] create node error. path:%s, err:%v\n", nodePath, err)
		return
	} else if !isExist {
		z.createPersistent(typePath, true)
	}
	if _, err := z.zkConn.Create(nodePath, []byte(url.ToExtInfo()), zk.FlagEphemeral, zk.WorldACL(zk.PermAll)); err != nil {
		vlog.Errorf("[ZkRegistry] create node error. path:%s, err:%v\n", nodePath, err)
		return
	}
}

func (z *ZkRegistry) createPersistent(path string, createParents bool) {
	if _, err := z.zkConn.Create(path, nil, 0, zk.WorldACL(zk.PermAll)); err != nil {
		if err == zk.ErrNoNode && createParents {
			parts := strings.Split(path, "/")
			parentPath := strings.Join(parts[:len(parts)-1], "/")
			z.createPersistent(parentPath, createParents)
			z.createPersistent(path, createParents)
			return
		}
		vlog.Errorf("[ZkRegistry] create persistent error. path:%s, err:%v\n", path, err)
	}
}

func getNodeInfo(data []byte) string {
	if len(data) > 7 && binary.BigEndian.Uint16(data[:2]) == streamMagicTag {
		if data[4] == shortStringTag {
			return string(data[7:])
		} else if data[4] == longStringTag && len(data) > 13 {
			return string(data[13:])
		}
	}
	return string(data)
}

func (z *ZkRegistry) nodeChildsToURLs(url *motan.URL, parentPath string, currentChilds []string) []*motan.URL {
	urls := make([]*motan.URL, 0, len(currentChilds))
	if currentChilds != nil {
		for _, node := range currentChilds {
			nodePath := parentPath + zkPathSeparator + node
			data, _, err := z.zkConn.Get(nodePath)
			if err != nil {
				vlog.Errorln("[ZkRegistry] get node data error. err:" + err.Error())
				continue
			}
			newURL := &motan.URL{}
			nodeInfo := getNodeInfo(data)
			if nodeInfo != "" {
				newURL = motan.FromExtInfo(nodeInfo)
			} else {
				newURL = url.Copy()
				var host string
				port := 80
				if strings.Index(node, ":") > -1 {
					hp := strings.Split(node, ":")
					if len(hp) > 1 {
						host = hp[0]
						port, _ = strconv.Atoi(hp[1])
					}
				} else {
					host = node
				}
				newURL.Host = host
				newURL.Port = port
			}
			if newURL.Port != 0 || newURL.Host != "" {
				urls = append(urls, newURL)
			}
		}
	}
	return urls
}

func toGroupPath(url *motan.URL) string {
	return zkRegistryNamespace + zkPathSeparator + url.Group
}

func toServicePath(url *motan.URL) string {
	return toGroupPath(url) + zkPathSeparator + url.Path
}

func toCommandPath(url *motan.URL) string {
	return toGroupPath(url) + zkRegistryCommand
}

func toNodeTypePath(url *motan.URL, nodeType string) string {
	return toServicePath(url) + zkPathSeparator + nodeType
}

func toNodePath(url *motan.URL, nodeType string) string {
	return toNodeTypePath(url, nodeType) + zkPathSeparator + url.GetAddressStr()
}

func toAgentPath(url *motan.URL) string {
	return zkRegistryNamespace + zkPathSeparator + zkNodeTypeAgent + zkPathSeparator + url.GetParam(motan.ApplicationKey, "")
}

func toAgentNodeTypePath(url *motan.URL) string {
	return toAgentPath(url) + zkRegistryNode
}

func toAgentNodePath(url *motan.URL) string {
	return toAgentNodeTypePath(url) + zkPathSeparator + url.GetAddressStr()
}

func toAgentCommandPath(url *motan.URL) string {
	return toAgentPath(url) + zkRegistryCommand
}
