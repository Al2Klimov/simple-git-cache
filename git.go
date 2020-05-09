package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/kataras/iris"
	irisCtx "github.com/kataras/iris/context"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const gitMirrorPath = "mirrors"
const tempDir = "tmp"

var tempChild = path.Join(tempDir, "*")
var execSemaphore = semaphore.NewWeighted(int64(runtime.GOMAXPROCS(0)) * 2)

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

	app    *iris.Application
	scheme string
	host   string
	port   string
}

func (vs *vServer) shutdown() {
	_ = vs.app.Shutdown(context.Background())
}

var statusCode = regexp.MustCompile(`\A(\d+)`)

func (vs *vServer) handler(ctx irisCtx.Context) {
	segments := strings.Split(ctx.Path(), "/")
	thold := -1

	for i := len(segments); i > 0; {
		i--

		if strings.HasSuffix(segments[i], ".git") {
			thold = i
			break
		}
	}

	if thold == -1 {
		ctx.StatusCode(400)
		return
	}

	thold++

	repoUrl := url.URL{
		Scheme: vs.scheme,
		Host:   string(vs.vListener.addr),
		Path:   strings.Join(segments[:thold], "/"),
	}

	gitPath, ok := runCmd("git", "--exec-path")
	if !ok {
		ctx.StatusCode(500)
		return
	}

	local := ensureGit(repoUrl.String())
	if local == "" {
		ctx.StatusCode(500)
		return
	}

	cgi := exec.Command(path.Join(string(bytes.TrimRight(gitPath, "\n")), "git-http-backend"))
	cgi.Stdin = ctx.Request().Body

	out, errOt := cgi.StdoutPipe()
	if errOt != nil {
		log.WithFields(log.Fields{"error": errOt.Error()}).Error("Couldn't create pipe")
		ctx.StatusCode(500)
		return
	}

	var err bytes.Buffer
	cgi.Stderr = &err

	uri := "/" + strings.Join(segments[thold:], "/")
	cgi.Env = append(
		os.Environ(),
		"GATEWAY_INTERFACE=CGI/1.1",
		"REQUEST_METHOD="+ctx.Method(),
		"SCRIPT_NAME="+uri,
		"PATH_INFO="+uri,
		"QUERY_STRING="+ctx.Request().URL.RawQuery,
		"SERVER_PROTOCOL="+ctx.Request().Proto,
		"CONTENT_TYPE="+ctx.GetHeader("Content-Type"),
		"CONTENT_LENGTH="+ctx.GetHeader("Content-Length"),
		"SERVER_SOFTWARE=simple-git-cache/1",
		"SERVER_NAME="+vs.host,
		"SERVER_PORT="+vs.port,
		"REMOTE_ADDR="+ctx.RemoteAddr(),
		"GIT_PROJECT_ROOT="+local,
		"GIT_HTTP_EXPORT_ALL=1",
	)

	for k, vs := range ctx.Request().Header {
		cgi.Env = append(cgi.Env, fmt.Sprintf(
			"HTTP_%s=%s", strings.ToUpper(strings.Replace(k, "-", "_", -1)), strings.Join(vs, ","),
		))
	}

	onTerm.RLock()
	defer onTerm.RUnlock()

	_ = execSemaphore.Acquire(context.Background(), 1)
	defer execSemaphore.Release(1)

	log.WithFields(log.Fields{"exe": cgi.Path}).Debug("Running command")
	if errSt := cgi.Start(); errSt != nil {
		log.WithFields(log.Fields{"exe": cgi.Path, "error": errSt.Error()}).Error("Command failed")
	}

	buf := bufio.NewReader(out)
	headers := map[string][]string{}

	for {
		line, errRB := buf.ReadBytes('\n')
		if errRB != nil {
			log.WithFields(log.Fields{"error": errRB.Error()}).Error("Couldn't read headers")

			if errWt := cgi.Wait(); errWt != nil {
				log.WithFields(log.Fields{
					"exe": cgi.Path, "error": errWt.Error(), "stderr": err.String(),
				}).Error("Command failed")
			}

			ctx.StatusCode(500)
			return
		}

		line = bytes.TrimRight(line, "\r\n")

		if len(line) < 1 {
			break
		}

		if kv := bytes.SplitN(line, []byte{':'}, 2); len(kv) == 2 {
			k := strings.ToLower(string(kv[0]))
			headers[k] = append(headers[k], string(bytes.TrimLeft(kv[1], " ")))
		}
	}

	if match := statusCode.FindStringSubmatch(strings.Join(headers["status"], ",")); match != nil {
		if sc, errPU := strconv.ParseUint(match[1], 10, 64); errPU == nil {
			ctx.StatusCode(int(sc))
		}
	}

	delete(headers, "status")

	for k, vs := range headers {
		for _, v := range vs {
			ctx.Header(k, v)
		}
	}

	_, _ = io.Copy(ctx, buf)

	if errWt := cgi.Wait(); errWt != nil {
		log.WithFields(log.Fields{
			"exe": cgi.Path, "error": errWt.Error(), "stderr": err.String(),
		}).Error("Command failed")
	}
}

func newVServer(upstream, scheme, host, port string) *vServer {
	srv := &vServer{
		vListener{sync.RWMutex{}, vAddr(upstream), make(chan net.Conn), make(chan struct{})},
		iris.Default(),
		scheme,
		host,
		port,
	}

	srv.app.Any("/{path:path}", srv.handler)

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

var gitLocks = struct {
	sync.RWMutex

	locks map[[sha256.Size]byte]*sync.Mutex
}{
	locks: map[[32]byte]*sync.Mutex{},
}

func ensureGit(remote string) string {
	hash := sha256.New()
	hash.Write([]byte(remote))

	var hashArray [sha256.Size]byte
	copy(hashArray[:], hash.Sum(nil))

	local := path.Join(gitMirrorPath, hex.EncodeToString(hashArray[:]))
	log.WithFields(log.Fields{"remote": remote, "local": local}).Info("Fetching Git repo")

	if !mkDir(tempDir) {
		return ""
	}

	if !mkDir(gitMirrorPath) {
		return ""
	}

	gitLocks.RLock()
	lock, ok := gitLocks.locks[hashArray]
	gitLocks.RUnlock()

	if !ok {
		gitLocks.Lock()

		lock, ok = gitLocks.locks[hashArray]
		if !ok {
			lock = &sync.Mutex{}
			gitLocks.locks[hashArray] = lock
		}

		gitLocks.Unlock()
	}

	lock.Lock()
	defer lock.Unlock()

	if _, errSt := os.Stat(local); errSt != nil {
		if os.IsNotExist(errSt) {
			log.WithFields(log.Fields{"local": local}).Debug("Initializing Git repo")

			git := mkTemp()
			if git == "" {
				return ""
			}

			defer rmDir(git)

			if _, ok := runCmd("git", "-C", git, "init", "--bare"); !ok {
				return ""
			}

			if _, ok := runCmd("git", "-C", git, "remote", "add", "--mirror=fetch", "--", "origin", remote); !ok {
				return ""
			}

			if !rename(git, local) {
				return ""
			}
		} else {
			log.WithFields(log.Fields{"path": local, "error": errSt.Error()}).Error("Stat error")
			return ""
		}
	}

	if _, ok := runCmd("git", "-C", local, "fetch", "origin"); !ok {
		return ""
	}

	return local
}

func mkDir(dir string) bool {
	log.WithFields(log.Fields{"path": dir}).Debug("Creating dir")

	if errMA := os.MkdirAll(dir, 0700); errMA != nil {
		log.WithFields(log.Fields{"path": dir, "error": errMA}).Error("Couldn't create dir")
		return false
	}

	return true
}

func mkTemp() string {
	log.WithFields(log.Fields{"path": tempChild}).Trace("Creating temp dir")

	dir, errTD := ioutil.TempDir(tempDir, "")
	if errTD != nil {
		log.WithFields(log.Fields{"path": tempChild, "error": errTD.Error()}).Error("Couldn't create temp dir")
		dir = ""
	}

	return dir
}

func rmDir(dir string) {
	log.WithFields(log.Fields{"path": dir}).Trace("Removing dir")

	if errRA := os.RemoveAll(dir); errRA != nil {
		log.WithFields(log.Fields{"path": dir, "error": errRA.Error()}).Warn("Couldn't remove dir")
	}
}

func runCmd(name string, arg ...string) (stdout []byte, ok bool) {
	cmd := exec.Command(name, arg...)
	var out, err bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &err

	onTerm.RLock()
	_ = execSemaphore.Acquire(context.Background(), 1)

	log.WithFields(log.Fields{"exe": name, "args": arg}).Debug("Running command")
	errRn := cmd.Run()

	execSemaphore.Release(1)
	onTerm.RUnlock()

	if errRn != nil {
		log.WithFields(log.Fields{
			"exe": name, "args": arg, "error": errRn.Error(), "stdout": out.String(), "stderr": err.String(),
		}).Error("Command failed")

		return nil, false
	}

	return out.Bytes(), true
}

func rename(old, new string) bool {
	log.WithFields(log.Fields{"old": old, "new": new}).Trace("Renaming")

	if errRn := os.Rename(old, new); errRn != nil {
		log.WithFields(log.Fields{"old": old, "new": new, "error": errRn.Error()}).Error("Couldn't rename")
		return false
	}

	return true
}
