package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"

	"github.com/uber/makisu/lib/log"
	"github.com/apourchet/commander"
	"go.uber.org/atomic"
)

// ListenFlags contains all of the flags for `makisu listen ...`
type ListenFlags struct {
	SocketPath string `commander:"flag=s,The absolute path of the unix socket that makisu will listen on"`
	building   *atomic.Bool
}

func newListenFlags() ListenFlags {
	return ListenFlags{
		SocketPath: "/makisu-socket/makisu.sock",
		building:   atomic.NewBool(false),
	}
}

// BuildRequest is the expected structure of the JSON body of http requests coming in on the socket.
// Example body of a BuildRequest:
//    ["build", "-t", "myimage:latest", "/context"]
type BuildRequest []string

// Listen creates the directory structures and the makisu socket, then it
// starts accepting http requests on that socket.
func (cmd ListenFlags) Listen() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", cmd.ready)
	mux.HandleFunc("/build", cmd.build)

	if err := os.MkdirAll(path.Dir(cmd.SocketPath), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create directory to socket %s: %v", cmd.SocketPath, err)
	}

	lis, err := net.Listen("unix", cmd.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket %s: %v", cmd.SocketPath, err)
	}
	log.Infof("Listening for build requests on unix socket %s", cmd.SocketPath)

	server := http.Server{Handler: mux}
	if err := server.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve on unix socket: %v", err)
	}
	return nil
}

func (cmd ListenFlags) ready(rw http.ResponseWriter, req *http.Request) {
	if cmd.building.Load() {
		rw.WriteHeader(http.StatusConflict)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

func (cmd ListenFlags) build(rw http.ResponseWriter, req *http.Request) {
	if ok := cmd.building.CAS(false, true); !ok {
		rw.WriteHeader(http.StatusConflict)
		rw.Write([]byte("Already processing a request"))
		return
	}
	defer cmd.building.Store(false)

	log.Infof("Serving build request")
	args := &BuildRequest{}
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, "%s\n", err.Error())
		return
	} else if err := json.Unmarshal(body, args); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, "%s\n", err.Error())
		return
	}

	r, newStderr, err := os.Pipe()
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(rw, "%s\n", err.Error())
		return
	}

	log.Infof("Piping stdout to response")
	oldLogger := log.GetLogger()
	os.Stderr = newStderr
	done := make(chan bool, 0)

	defer func() {
		newStderr.Close()
		<-done
		log.SetLogger(oldLogger)
		log.Infof("Build request served")
	}()

	go func() {
		defer func() { done <- true }()
		reader := bufio.NewReader(r)
		for {
			line, _, err := reader.ReadLine()
			if err == io.EOF {
				return
			} else if err != nil {
				return
			}
			line = append(line, '\n')
			rw.Write(line)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
		}
	}()

	rw.WriteHeader(http.StatusOK)
	log.Infof("Starting build")

	commander := commander.New()
	commander.FlagErrorHandling = flag.ContinueOnError
	app, err := NewApplication()
	if err != nil {
		log.Errorf("%v", err)
		return
	} else if err := commander.RunCLI(app, *args); err != nil {
		log.Errorf("%v", err)
		return
	} else if err := app.Cleanup(); err != nil {
		log.Errorf("%v", err)
		return
	}
}