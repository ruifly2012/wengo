package network

import (
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/panjf2000/ants"
	"github.com/wengo/appdata"
	"github.com/wengo/csvdata"
	"github.com/wengo/model"
	"github.com/wengo/timersys"
	"github.com/wengo/xlog"
	"net"
	"sync"
	"time"
)

var (
	connIDs *model.IncrementId // 自增id生成器,用id，有利于连接在其他地方使用，降低包的依赖
)

func init() {
	connIDs = model.NewIncrementId()
	fmt.Println("connID已经初始化")
}

func Release() {
	if connIDs != nil {
		connIDs.Release() // 清空列表
	}
}

type TCPServer struct {
	ln           net.Listener                // 服务器监听对象
	connetsize   *model.AtomicInt32FlagModel // 已经连接的数量
	connSyncmaps sync.Map                    // id 加 连接对象
	//connm        map[uint32]*TcpConn      就用sync.map保证效率高点
	//mutexConns   sync.Mutex
	wgLn        sync.WaitGroup
	wgConns     sync.WaitGroup
	netObserver NetWorkObserver // 网络事件观察者
	netConf     *csvdata.Networkconf
	workPool    *ants.Pool // 协程池
	linkTicker  *timersys.TimeTicker
	writeChan   chan *GroupMessage // 写的通道，我服务器写的消息先写入通道再用连接传出去
	stopEvent   chan bool
	msgPool     sync.Pool
	isClose     *model.AtomicBool //是否关闭
	msgParser   *MsgParser        //消息解析
}

// 创建tcp Sever服务器
func NewTcpServer(netobs NetWorkObserver, conf *csvdata.Networkconf, pool *ants.Pool) *TCPServer {
	if conf == nil {
		xlog.ErrorLogNoInScene("server conf is nil")
		return nil
	}
	tcpsv := new(TCPServer)
	tcpsv.netObserver = netobs
	tcpsv.netConf = conf
	// 协程池,这里要为每个连接开读写线程
	tcpsv.workPool = pool
	tcpsv.connetsize = model.NewAtomicInt32Flag()
	tcpsv.isClose = model.NewAtomicBool()
	tcpsv.isClose.SetFalse()
	
	tcpsv.writeChan = make(chan *GroupMessage, conf.Write_cap_num)
	tcpsv.stopEvent = make(chan bool, 1)
	tcpsv.msgParser = NewMsgParser(conf.Msglen_size, conf.Max_msglen, conf.Msg_isencrypt)
	tcpsv.msgPool.New = func() interface{} {
		return new(GroupMessage)
	}
	return tcpsv
}

func (server *TCPServer) Start() {
	xlog.DebugLogNoInScene("TCPServer start")
	server.init()
	go server.run()
	if !server.isInnerServer() {
		server.linkTicker = timersys.NewTimeTicker(time.Second, server.checkLink)
	}
	go server.serverEvent()
}


//服务器内部通信不需要检测
func (this *TCPServer) isInnerServer() bool {
	return this.netConf.App_kind == model.APP_DataCenter || this.netConf.App_kind == model.APP_GameServer
}

func (server *TCPServer) init() {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%s", server.netConf.Out_prot))
	if err != nil {
		xlog.DebugLogNoInScene("%v", err)
		panic(err)
	}
	xlog.DebugLogNoInScene("TCPServer listen Addr:%v", ln.Addr())
	
	if server.netConf.Max_connect <= 0 {
		server.netConf.Max_connect = 100000
		xlog.WarningLogNoInScene("invalid MaxConnNum, reset to %v", server.netConf.Max_connect)
	}
	if server.netConf.Write_cap_num <= 0 {
		server.netConf.Write_cap_num = 200
		xlog.WarningLogNoInScene("invalid PendingWriteNum, reset to %v", server.netConf.Write_cap_num)
	}
	// if server.NewAgent == nil {
	// 	xlog.WarningLog(appdata.GetSecenName(),"NewAgent must not be nil")
	// }
	
	server.ln = ln
}

func (server *TCPServer) run() {
	server.wgLn.Add(1)
	defer server.wgLn.Done()
	var tempDelay time.Duration
	for {
		conn, err := server.ln.Accept()
		if err != nil {
			// 临时错误才继续，其他错误就关闭监听
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				xlog.WarningLogNoInScene("accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			xlog.WarningLogNoInScene("TCPServer Accept erro:%v", err)
			return
		}
		tempDelay = 0
		// 添加连接
		if !server.addConn(conn) {
			continue
		}
	}
}

// 添加链接信息
func (server *TCPServer) addConn(conn net.Conn) bool {
	
	if server.GetConnectSize() >= int32(server.netConf.Max_connect) {
		erro := conn.Close()
		if erro != nil {
			xlog.WarningLogNoInScene("超过连接关闭链接错误 %v ", erro)
		}
		xlog.WarningLogNoInScene("超过最大链接数,当前连接数%d", server.connetsize)
		return false
	}
	// 创建封装的连接
	ConnID := connIDs.GetId()
	tcpConn := newTcpConn(conn, ConnID, server.netObserver, server.netConf, server.workPool, server.msgParser)
	server.connSyncmaps.Store(ConnID, tcpConn) // 存储连接
	server.connetsize.AddInt32()
	server.wgConns.Add(1)
	// 连接接收消息由于连接是动态最好用
	server.workPool.Submit(func() {
		server.ReceiveData(tcpConn)
	})
	xlog.DebugLogNoInScene("当前连接数addConn %d,连接标识%d", server.GetConnectSize(),ConnID)
	return true
}

// 连接中读取数据
func (server *TCPServer) ReceiveData(tcpConn *TcpConn) {
	defer server.wgConns.Done() // 关闭连接waitgorup减一

	defer xlog.RecoverToLog(func() {
		// 出錯要关闭远端连接
		server.closeConn(tcpConn)
	})
	for {
		err := tcpConn.ReadMsg()
		if err != nil { // 这里读到错误消息,关闭
			xlog.WarningLogNoInScene("read message err: %v ", err)
			break // 关闭连接
		}
	}
	// 处理远端关闭
	server.closeConn(tcpConn)
}

// 根据连接id断开连接
func (server *TCPServer) CloseConnID(connID uint32) {
	conn := server.GetTcpConnet(connID)
	if conn != nil { //让写协程关闭,这样流程才正确 保证closeConn只被调用一次
		conn.Close()
	}
}

// 被动断开连接 read EOF
func (server *TCPServer) closeConn(tcpConn *TcpConn) {
	if tcpConn == nil {
		return
	}

	tcpConn.Close() // 关闭写协程
	id := tcpConn.GetConnID()
	server.connSyncmaps.Delete(id)
	server.connetsize.SubInt32()
	xlog.DebugLogNoInScene("被动断开连接 当前连接数closeConn %d,移除连接标识%d", server.GetConnectSize(),id)
}

// 获取连接数
func (server *TCPServer) GetConnectSize() int32 {
	return server.connetsize.GetInt32()
}

//监测连接数
func (server *TCPServer) checkLink() {
	//xlog.WarningLogNoInScene("TCPServercheckLink")
	// 关闭所有连接
	server.connSyncmaps.Range(func(key, value interface{}) bool {
		conn, ok := value.(*TcpConn)
		if ok {
			if !conn.IsAlive() {
				xlog.ErrorLogNoInScene("connid %v 已经超过 %v 秒没发包", conn.GetConnID(), server.netConf.Checklink_s)
				conn.Close()
			}
		}
		return true
	})
}

// 获取连接数
func (server *TCPServer) serverEvent() {
	
	for {
		select {
		case groupmsg := <-server.writeChan: // 写的
			if groupmsg == nil {
				break
			}
			server.doSend(groupmsg)
		case <-server.stopEvent: // 停止事件
			close(server.stopEvent)
			close(server.writeChan)
			return //退出协程
		}
	}
	// xlog.DebugLogNoInScene("tcp serverEvent")
}

func (server *TCPServer) Close() {
	server.isClose.SetTrue()
	if server.linkTicker != nil { //只有开启了才关闭
		server.linkTicker.StopTicker()
	}
	server.stopEvent <- true // 关闭服务器事件
	
	erro := server.ln.Close() // 关闭监听
	if erro != nil {
		xlog.WarningLogNoInScene("TCPServer关闭监听错误 %v", erro)
	}
	server.wgLn.Wait()
	// 关闭所有连接
	server.connSyncmaps.Range(func(key, value interface{}) bool {
		conn, ok := value.(*TcpConn)
		if ok {
			conn.Close()
		}
		return true
	})
	server.wgConns.Wait()
	fmt.Println("TCPServer doClose")
}

// 发送多个消息
func (server *TCPServer) WriteMoreMsgByConnID(ConnID uint32, msg ...[]byte) error {
	tcpconn := server.GetTcpConnet(ConnID)
	if tcpconn == nil {
		return errors.New(fmt.Sprintf("SendMsg 未找到连接 %v", ConnID))
	}
	return tcpconn.WriteMsg(msg...)
}

// 写单个消息
func (server *TCPServer) WriteOneMsgByConn(conn Conner, maincmd, subcmd uint16, msg []byte) error {
	return server.WriteOneMsgByConnID(conn.GetConnID(), maincmd, subcmd, msg)
}

// 写单个消息
func (server *TCPServer) WriteOneMsgByConnID(ConnID uint32, maincmd, subcmd uint16, msg []byte) error {
	tcpconn := server.GetTcpConnet(ConnID)
	if tcpconn == nil {
		return errors.New(fmt.Sprintf("SendMsg 未找到连接 %v", ConnID))
	}
	return tcpconn.WriteOneMsg(maincmd, subcmd, msg)
}

// 用protubuf的方式写单个消息
func (server *TCPServer) WritePBMsgByConn(conn Conner, maincmd, subcmd uint16, pb proto.Message) error {
	ConnID := conn.GetConnID()
	tcpconn := server.GetTcpConnet(ConnID)
	if tcpconn == nil {
		return errors.New(fmt.Sprintf("SendMsg 未找到连接 %v", ConnID))
	}
	return tcpconn.WritePBMsg(maincmd, subcmd, pb)
}

//  用protubuf的方式写单个消息
func (server *TCPServer) WritePBMsgByConnID(ConnID uint32, maincmd, subcmd uint16, pb proto.Message) error {
	tcpconn := server.GetTcpConnet(ConnID)
	if tcpconn == nil {
		return errors.New(fmt.Sprintf("SendMsg 未找到连接 %v", ConnID))
	}
	return tcpconn.WritePBMsg(maincmd, subcmd, pb)
}

// 根据命令及protobuf创建包
func (server *TCPServer) CreatePBMsg(maincmd, subcmd uint16, pb proto.Message) (sendMsg []byte, erro error) {
	if pb != nil {
		sendMsg, erro = proto.Marshal(pb)
	}
	if erro != nil {
		xlog.ErrorLog(appdata.GetSecenName(), "CreatePBMsg %v", erro)
		return nil, erro
	}
	sendMsg, erro = server.CreatePackage(maincmd, subcmd, sendMsg)
	return
}

// 根据命令创建包
func (server *TCPServer) CreatePackage(maincmd, subcmd uint16, msg []byte) ([]byte, error) {
	return server.msgParser.PackOne(maincmd, subcmd, msg)
}

// 将多个包合并成一个
func (server *TCPServer) MorePackageToOne(args ...[]byte) ([]byte, error) {
	return server.msgParser.MorePackageToOne(args...)
}

// 给所有连接发送消息
func (server *TCPServer) SendAllConn(msg []byte) {
	if server.isClose.IsTrue() {
		return
	}
	groupmsg := server.createGroupMessage(nil, msg)
	if groupmsg == nil {
		return
	}
	server.writeChan <- groupmsg
}

// 给所有连接发送消息
func (server *TCPServer) SendSomeConn(ConnIDs []uint32, msg []byte) {
	if server.isClose.IsTrue() {
		return
	}
	groupmsg := server.createGroupMessage(ConnIDs, msg)
	if groupmsg == nil {
		return
	}
	server.writeChan <- groupmsg
}

// 从池子里面创建消息
func (server *TCPServer) createGroupMessage(ConnIDs []uint32, msg []byte) *GroupMessage {
	if msg == nil {
		xlog.ErrorLogNoInScene("群发消息为空")
		return nil
	}
	groupval := server.msgPool.Get()
	if groupval == nil {
		xlog.ErrorLogNoInScene("获取消息体错误")
		return nil
	}
	groupmsg, ok := groupval.(*GroupMessage)
	if !ok {
		xlog.ErrorLogNoInScene("获取消息体错误")
		return nil
	}
	groupmsg.Msgdata = msg
	groupmsg.ConnIDs = ConnIDs
	return groupmsg
}

func (server *TCPServer) doSend(message *GroupMessage) {
	if message == nil || message.Msgdata == nil {
		xlog.ErrorLogNoInScene("群发消息为空")
		return
	}
	// 发送给一部分
	if message.ConnIDs != nil && len(message.ConnIDs) > 0 {
		for _, connId := range message.ConnIDs {
			erro := server.sendOneMsg(connId, message.Msgdata)
			if erro != nil {
				xlog.DebugLogNoInScene("connId 发送消息错误 %v", connId)
			}
		}
		return
	}
	
	// 发送给全部连接
	server.connSyncmaps.Range(func(key, value interface{}) bool {
		conn, ok := value.(*TcpConn)
		if ok {
			conn.Write(message.Msgdata)
		}
		return true
	})
}

// 写单个消息
func (server *TCPServer) sendOneMsg(ConnID uint32, msg []byte) error {
	tcpconn := server.GetTcpConnet(ConnID)
	if tcpconn == nil {
		return errors.New(fmt.Sprintf("SendMsg 未找到连接 %v", ConnID))
	}
	// 向写通道投递数据
	tcpconn.Write(msg)
	return nil
}

//获取tcp连接
func (server *TCPServer) GetTcpConnet(ConnID uint32) *TcpConn {
	conn, ok := server.connSyncmaps.Load(ConnID)
	if !ok {
		xlog.ErrorLogNoInScene("TCPServer GetTcpConnet 连接标识%v已经关闭", ConnID)
		return nil
	}
	tcpconn, ok := conn.(*TcpConn)
	if !ok {
		xlog.ErrorLogNoInScene("转换 *TcpConn 失败")
		return nil
	}
	return tcpconn
}
