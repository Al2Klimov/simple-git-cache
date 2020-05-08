package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	irisCtx "github.com/kataras/iris/context"
	log "github.com/sirupsen/logrus"
	"math/big"
	"net"
)

var tlsOffload = func() tls.Config {
	priv, errGK := rsa.GenerateKey(rand.Reader, 1024)
	if errGK != nil {
		panic(errGK)
	}

	template := x509.Certificate{SerialNumber: big.NewInt(0)}
	cert, errCC := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)

	if errCC != nil {
		panic(errCC)
	}

	return tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{cert},
		PrivateKey:  priv,
	}}}
}()

func proxy(ctx irisCtx.Context) {
	uri := ctx.Request().RequestURI
	_, port, errSA := net.SplitHostPort(uri)

	if errSA != nil {
		ctx.StatusCode(400)
		_, _ = ctx.Write([]byte(errSA.Error()))
		return
	}

	wrapTls := false

	switch port {
	case "80":
	case "443":
		wrapTls = true
	default:
		ctx.StatusCode(403)
		return
	}

	ctx.StatusCode(200)
	if _, errWr := ctx.Write(nil); errWr != nil {
		return
	}

	conn, _, errHj := ctx.ResponseWriter().Hijack()
	if errHj != nil {
		log.WithFields(log.Fields{"error": errHj.Error()}).Error("Couldn't hijack HTTP connection")
		return
	}

	if wrapTls {
		tlsConn := tls.Server(conn, &tlsOffload)
		conn = tlsConn

		if errHs := tlsConn.Handshake(); errHs != nil {
			log.WithFields(log.Fields{"error": errHs.Error()}).Error("TLS handshake failed")
			conn.Close()
			return
		}
	}

	vServers.RLock()
	srv, ok := vServers.perUri[uri]
	vServers.RUnlock()

	if !ok {
		vServers.Lock()

		if srv, ok = vServers.perUri[uri]; !ok {
			srv = newVServer(uri)
			vServers.perUri[uri] = srv
		}

		vServers.Unlock()
	}

	if !srv.dial(conn) {
		conn.Close()
	}
}
