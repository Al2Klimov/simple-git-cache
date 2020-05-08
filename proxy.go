package main

import (
	irisCtx "github.com/kataras/iris/context"
	log "github.com/sirupsen/logrus"
	"net"
)

func proxy(ctx irisCtx.Context) {
	_, port, errSA := net.SplitHostPort(ctx.Request().RequestURI)

	if errSA != nil {
		ctx.StatusCode(400)
		_, _ = ctx.Write([]byte(errSA.Error()))
		return
	}

	switch port {
	case "80", "443":
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

	defer conn.Close()
}
