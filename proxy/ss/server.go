/*
	Author moli
	基于kcp/udp/tcp协议的shadowsocks服务器端
	* 代码有参考学习shadowsocks-go、kcptun的源码
*/

package ss

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/xtaci/kcp-go"
	//"github.com/xtaci/smux"

	"bitproxy/services"
	"bitproxy/utils"
)

type SSServer struct {
	crypt       string
	pwd         string
	port        uint
	channel_net NetProtocol //客户端与服务器端的通信协议 tcp/udp/kcp
	rate        uint
	ln          net.Listener
	log         *utils.Logger
	done        bool
}

func (this *SSServer) getRequest(client io.Reader) (host string, extra []byte, err error) {
	buf := make([]byte, 260)
	var n int
	if n, err = client.Read(buf); err != nil {
		this.log.Info("Read request buf err ", err)
		err = errors.New("Read request buf err " + err.Error())
		return
	}

	reqLen := 0
	switch buf[SERVER_RAW_TYPE] {
	case TYPE_IPV4:
		reqLen = SERVER_LEN_TYPE_IPV4
	case TYPE_IPV6:
		reqLen = SERVER_LEN_TYPE_IPV6
	case TYPE_DOMAIN:
		reqLen = int(buf[SERVER_RAW_DOMAIN_LEN]) + SERVER_LEN_TYPE_DOMAIN_BASE
	default:
		this.log.Info("Raw addr err")
		err = errors.New("Raw addr err")
		return
	}

	if n < reqLen {
		// n 小于 reqLen 的长度，则认为传送过来的数据不完成，必须等待读取完整才能继续
		if _, err = io.ReadFull(client, buf[n:reqLen]); err != nil {
			return
		}
	} else if n > reqLen {
		// 如果取多了数据
		extra = buf[reqLen:n]
	}
	switch buf[SERVER_RAW_TYPE] {
	case TYPE_IPV4:
		host = net.IP(buf[IPV4_INDEX : IPV4_INDEX+net.IPv4len]).String()
	case TYPE_IPV6:
		host = net.IP(buf[IPV6_INDEX : IPV6_INDEX+net.IPv6len]).String()
	case TYPE_DOMAIN:
		host = string(buf[DOMAIN_INDEX : DOMAIN_INDEX+buf[SERVER_RAW_DOMAIN_LEN]])
	}

	port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
	host = net.JoinHostPort(host, fmt.Sprintf("%d", port))
	return
}

func (this *SSServer) handle(client net.Conn) {
	client, err := NewCryptConn(client, this.pwd, this.crypt)
	if err != nil {
		this.log.Info("NewCryptConn err ", err)
	}

	raw_remote_addr, extra, err := this.getRequest(client)
	if err != nil {
		this.log.Info("Request err ", err)
		return
	}

	remote, err := net.Dial("tcp", raw_remote_addr)
	if err != nil {
		this.log.Info("Dial to raw host err ", err, raw_remote_addr)
		return
	}

	if extra != nil {
		if _, err = remote.Write(extra); err != nil {
			this.log.Info("Write extra date err ", err)
			return
		}
	}
	limit := &utils.Limit{Rate: this.rate}
	var traffic_stats = func(n int64, e error) {
		services.AddTrafficStats(this.port, n)
	}
	go utils.Copy(client, remote, limit, nil, nil, nil, nil, traffic_stats, nil)
	utils.Copy(remote, client, nil, nil, nil, nil, nil, nil, nil)
}

func (this *SSServer) initListen() (err error) {
	err = nil
	if this.channel_net == KCP_PROTOCOL {
		this.ln, err = kcp.ListenWithOptions(this.addr(), nil, 10, 3)
		if err != nil {
			this.log.Info("kcp.DialWithOptions error", err)
			return errors.New("SSCLIENT: kcp.DialWithOptions error " + err.Error())
		}
		if err != nil {
			this.log.Info("Init kcp option err: ", err)
			return err
		}

	} else if this.channel_net == TCP_PROTOCOL {
		this.ln, err = net.Listen("tcp", this.addr())

	} else if this.channel_net == UDP_PROTOCOL {
		this.ln, err = net.Listen("udp", this.addr())
	} else {
		return errors.New("Not fount net type")
	}
	return
}

func (this *SSServer) AcceptClient() (net.Conn, error) {
	if this.channel_net == KCP_PROTOCOL {
		conn, err := this.ln.(*kcp.Listener).AcceptKCP()
		if err != nil {
			this.log.Info("Get kcp conn err: ", err)
			return nil, err
		}
		conn.SetStreamMode(true)
		conn.SetNoDelay(1, 20, 1, 0)
		conn.SetMtu(1350)
		conn.SetWindowSize(1024, 1024)
		conn.SetACKNoDelay(true)
		conn.SetKeepAlive(10)

		this.log.Info("Accept address:", conn.RemoteAddr())
		return conn, nil

	} else if this.channel_net == TCP_PROTOCOL {
		return this.ln.Accept()

	} else if this.channel_net == UDP_PROTOCOL {
		return this.ln.Accept()
	}
	return nil, errors.New("Not found conn")
}

func (this *SSServer) addr() string {
	return fmt.Sprintf(":%d", this.port)
}

func (this *SSServer) Start() error {
	err := this.initListen()
	if err != nil {
		this.log.Info("Listen by protocol err: ", err)
		return err
	}
	this.log.Info("Listen port", this.port)

	for {
		conn, err := this.AcceptClient()
		if err != nil {
			if this.done {
				break
			}
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				this.log.Info("Temporary error when accepting new connections: ", netErr)
				time.Sleep(time.Second)
				continue
			}
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				this.log.Info("Permanent error when accepting new connections: ", err)
				return err
			}
			if err != nil {
				this.log.Info("Accept err ", this.port, " ", err)
				continue
			}
		} else {
			this.log.Info("Accept address:", conn.(net.Conn).RemoteAddr())
			go this.handle(conn)
		}
	}
	return nil
}

func (this *SSServer) Stop() (err error) {
	if this.ln == nil {
		return nil
	}
	this.done = true
	if this.ln != nil {
		err = this.ln.Close()
		this.ln = nil
	}
	return
}

func (this *SSServer) Traffic() (uint64, error) {
	return services.GetTraffic(this.port)
}

func (this *SSServer) LocalPort() uint {
	return this.port
}

func NewServer(channel_net NetProtocol, port uint, pwd, crypt string, rate uint) *SSServer {
	return &SSServer{
		crypt:       crypt,
		pwd:         pwd,
		port:        port,
		channel_net: channel_net,
		rate:        rate,
		log:         utils.NewLogger("SSServer"),
	}
}
