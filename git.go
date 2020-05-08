package main

import (
	"context"
	"github.com/kataras/iris"
	"net"
	"sync"
	"syscall"
)

type vAddr string

var _ net.Addr = vAddr("")

func (vAddr) Network() string {
	return ""
}

func (va vAddr) String() string {
	return string(va)
}

type vListener struct {
	sync.RWMutex

	addr   vAddr
	conns  chan net.Conn
	closed chan struct{}
}

var _ net.Listener = (*vListener)(nil)

func (vl *vListener) Accept() (net.Conn, error) {
	conn, ok := <-vl.conns
	if !ok {
		return nil, syscall.EINVAL
	}

	return conn, nil
}

func (vl *vListener) Close() error {
	close(vl.closed)

	vl.Lock()
	defer vl.Unlock()

	close(vl.conns)

	return nil
}

func (vl *vListener) Addr() net.Addr {
	return vl.addr
}

func (vl *vListener) dial(conn net.Conn) bool {
	vl.RLock()
	defer vl.RUnlock()

	select {
	case <-vl.closed:
		return false
	default:
	}

	select {
	case vl.conns <- conn:
		return true
	case <-vl.closed:
		return false
	}
}

type vServer struct {
	vListener

	app *iris.Application
}

func (vs *vServer) shutdown() {
	_ = vs.app.Shutdown(context.Background())
}

func newVServer(upstream string) *vServer {
	srv := &vServer{
		vListener{sync.RWMutex{}, vAddr(upstream), make(chan net.Conn), make(chan struct{})},
		iris.Default(),
	}

	onTerm.RLock()
	onTerm.toDo = append(onTerm.toDo, srv.shutdown)
	go srv.app.Run(iris.Listener(&srv.vListener), iris.WithoutStartupLog, iris.WithoutInterruptHandler)
	onTerm.RUnlock()

	return srv
}

var vServers = struct {
	sync.RWMutex

	perUri map[string]*vServer
}{
	perUri: map[string]*vServer{},
}
