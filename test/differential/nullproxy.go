// Package differential 提供差分测试基建:零号代理、会话脚本播放器与结果对比器。
//
// 差分测试的基本形态:同一会话脚本分别在「直连 MySQL」与「经代理」两条路径上执行,
// 对每一步的观测(错误码、affected_rows、last_insert_id、结果集、warnings)做逐字段对比。
// 零号代理是校准基准——它只搬运字节,不做任何 MySQL 语义处理,穿过它的会话必须与
// 直连完全一致;若不一致,说明 harness 或零号代理自身有 bug。
package differential

import (
	"io"
	"net"
	"sync"
)

// NullProxy 是"零号代理":不做任何 MySQL 语义解析,仅双向搬运字节的 TCP 管道。
type NullProxy struct {
	ln      net.Listener
	backend string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  chan struct{}
}

// StartNullProxy 在 listenAddr(可为 "127.0.0.1:0" 由系统分配端口)启动零号代理,
// 后端转发到 backendAddr。
func StartNullProxy(listenAddr, backendAddr string) (*NullProxy, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	p := &NullProxy{
		ln:      ln,
		backend: backendAddr,
		conns:   make(map[net.Conn]struct{}),
		done:    make(chan struct{}),
	}
	go p.accept()
	return p, nil
}

// Addr 返回实际监听地址。
func (p *NullProxy) Addr() net.Addr { return p.ln.Addr() }

func (p *NullProxy) accept() {
	defer close(p.done)
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		backend, err := net.Dial("tcp", p.backend)
		if err != nil {
			client.Close()
			continue
		}
		p.track(client, backend)
		go pipe(backend, client) // client -> backend
		go pipe(client, backend) // backend -> client
	}
}

// pipe 单向搬运并在结束后传播半关闭(FIN),保证与直连行为一致。
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func (p *NullProxy) track(a, b net.Conn) {
	p.mu.Lock()
	p.conns[a] = struct{}{}
	p.conns[b] = struct{}{}
	p.mu.Unlock()
}

// Stop 关闭监听与所有活动连接;在途会话会被强制断开。
func (p *NullProxy) Stop() error {
	err := p.ln.Close()
	<-p.done
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	return err
}
