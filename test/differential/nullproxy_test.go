package differential

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNullProxyForwardsAndHalfCloses(t *testing.T) {
	// 后端是一个 echo 服务:原样回显,读到 EOF 后关闭。
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()

	p, err := StartNullProxy("127.0.0.1:0", backend.Addr().String())
	require.NoError(t, err)
	defer func() { _ = p.Stop() }()

	c, err := net.Dial("tcp", p.Addr().String())
	require.NoError(t, err)
	defer c.Close()

	msg := []byte("hello 零号代理")
	_, err = c.Write(msg)
	require.NoError(t, err)
	require.NoError(t, c.(*net.TCPConn).CloseWrite())

	got, err := io.ReadAll(c)
	require.NoError(t, err)
	require.Equal(t, msg, got, "穿过零号代理的字节应与写入完全一致")
}
