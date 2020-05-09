//go:generate go run github.com/Al2Klimov/go-gen-source-repos

package main

import (
	"context"
	"flag"
	"github.com/kataras/golog"
	"github.com/kataras/iris"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh/terminal"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type logLevelValue log.Level

var _ flag.Value = (*logLevelValue)(nil)

func (llv *logLevelValue) String() string {
	return log.Level(*llv).String()
}

func (llv *logLevelValue) Set(raw string) error {
	lvl, errPL := log.ParseLevel(raw)
	*llv = logLevelValue(lvl)
	return errPL
}

type tcpAddrValue string

var _ flag.Value = (*tcpAddrValue)(nil)

func (tav *tcpAddrValue) String() string {
	return string(*tav)
}

func (tav *tcpAddrValue) Set(s string) error {
	_, _, errSA := net.SplitHostPort(s)
	*tav = tcpAddrValue(s)
	return errSA
}

var onTerm struct {
	sync.RWMutex

	toDo []func()
}

func main() {
	listen := tcpAddrValue(":1080")
	flag.Var(&listen, "listen", "IP:PORT")

	level := logLevelValue(log.InfoLevel)
	flag.Var(&level, "log-level", "LEVEL")

	flag.Parse()

	initLogging(log.Level(level))
	go wait4term()

	log.WithFields(log.Fields{"projects": GithubcomAl2klimovGo_gen_source_repos}).Info(
		"For the terms of use, the source code and the authors see the projects this program is assembled from",
	)

	log.WithFields(log.Fields{"local": string(listen)}).Info("Listening")

	server, errLs := net.Listen("tcp", string(listen))
	if errLs != nil {
		log.WithFields(log.Fields{"local": string(listen), "error": errLs.Error()}).Fatal("Couldn't listen")
	}

	app := iris.Default()
	app.Connect("/", proxy)

	onTerm.Lock()
	onTerm.toDo = append(onTerm.toDo, func() {
		_ = app.Shutdown(context.Background())
	})
	onTerm.Unlock()

	_ = app.Run(iris.Listener(server), iris.WithoutStartupLog, iris.WithoutInterruptHandler)
}

func initLogging(lvl log.Level) {
	if !terminal.IsTerminal(int(os.Stdout.Fd())) {
		log.SetFormatter(&log.JSONFormatter{})
	}

	log.SetOutput(os.Stdout)
	log.SetLevel(lvl)

	golog.InstallStd(log.StandardLogger())
	golog.SetLevel("debug")
}

func wait4term() {
	ch := make(chan os.Signal, 1)

	{
		signals := [2]os.Signal{syscall.SIGTERM, syscall.SIGINT}
		signal.Notify(ch, signals[:]...)
		log.WithFields(log.Fields{"signals": signals}).Trace("Listening for signals")
	}

	log.WithFields(log.Fields{"signal": <-ch}).Info("Terminating")

	onTerm.Lock()
	for _, f := range onTerm.toDo {
		f()
	}

	os.Exit(0)
}
