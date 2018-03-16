package main

/*
* daemonization:
* + is a OS level question, not a programming language level question
* + using init systems like systemd, launchd, daemontools, supervisor,
*   runit, Kubernetes, heroku, Borg, etc etc
* - https://github.com/sevlyar/go-daemon
 */

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/posteo/go-agentx"
	"github.com/posteo/go-agentx/pdu"
	"github.com/posteo/go-agentx/value"
	"gopkg.in/errgo.v1"
	"gopkg.in/yaml.v2"
)

type Connect struct {
	Host    string `yaml:"host"`
	Prot    string `yaml:"prot"`
	Port    string `yaml:"port"`
	Timeout int64  `yaml:"timeout"`
	Retry   struct {
		Period int64 `yaml:"period"`
		Count  int64 `yaml:"count"`
	} `yaml:"retry"`
}

type Health struct {
	Port string `yaml:"port"`
}

type Item struct {
	Meta struct {
		Name   string `yaml:"name"`
		Type   string `yaml:"type"`
		Vendor string `yaml:"vendor"`
	} `yaml:"metadata"`
	Poll struct {
		Timeout int64 `yaml:"timeout"`
		Period  int64 `yaml:"period"`
	} `yaml:"poll"`
	OID struct {
		Index string `yaml:"index"`
		Value string `yaml:"value"`
	} `yaml:"oid"`
	Exec struct {
		Index struct {
			Cmd  string   `yaml:"cmd"`
			Args []string `yaml:"args"`
		} `yaml:"index"`
		Value struct {
			Cmd  string   `yaml:"cmd"`
			Args []string `yaml:"args"`
		} `yaml:"value"`
	} `yaml:"exec"`
}

type Discovery struct {
	OID   string `yaml:"oid"`
	Items []Item `yaml:"items"`
}

type agentxConf struct {
	Connect   `yaml:"connect"`
	Health    `yaml:"health"`
	Discovery `yaml:"discovery"`
}

type cacheRow struct {
	Index string
	Value string
}

type globalCtx struct {
	session *agentx.Session
	wg      sync.WaitGroup
	done    chan bool
	baseOID string
}

type pollerCtx struct {
	globalCtx
	item Item
}

const (
	confFile = "agentx.yml"
)

func main() {
	cfg := agentxConf{}
	fn, _ := filepath.Abs(filepath.Dir(os.Args[0]) + "/" + confFile)

	fcfg, err := ioutil.ReadFile(fn)
	if err != nil {
		panic(err)
	}

	if err := yaml.Unmarshal(fcfg, &cfg); err != nil {
		panic(err)
	}

	session, err := getSession(&cfg.Connect)
	if err != nil {
		log.Fatalf(errgo.Details(err))
	}

	session.Handler = &agentx.ListHandler{}

	if err := session.Register(127, value.MustParseOID(cfg.Discovery.OID)); err != nil {
		log.Fatalf(errgo.Details(err))
	}

	done := make(chan bool)
	defer close(done)

	ctx := globalCtx{
		session: session,
		done:    done,
		baseOID: cfg.Discovery.OID}

	if err := setSignal(&ctx); err != nil {
		log.Fatalf(errgo.Details(err))
	}

	healthServerInit(&cfg)
	pollingLoop(&ctx, &cfg)
}

func setSignal(ctx *globalCtx) error {
	signal_chan := make(chan os.Signal, 1)
	signal.Notify(signal_chan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		code := 0
		break_loop := false
		for {
			s := <-signal_chan
			switch s {
			case syscall.SIGHUP:
				fmt.Println("hungup")

			case syscall.SIGINT:
				fmt.Println("Warikomi")

			case syscall.SIGTERM:
				fmt.Println("force stop")
				break_loop = true

			case syscall.SIGQUIT:
				fmt.Println("stop and core dump")
				break_loop = true

			default:
				fmt.Println("Unknown signal.")
				code = 1
				break_loop = true
			}
			if break_loop {
				break
			}
		}
		appCleanup(ctx)
		os.Exit(code)
	}()
	return nil
}

func appCleanup(ctx *globalCtx) {
	select {
	case <-time.After(1 * time.Second):
		log.Println("appCleanup: finished by timeout")
	case ctx.done <- true:
		log.Println("appCleanup: finished by done signal")
	}
}

func getSession(cfg *Connect) (*agentx.Session, error) {
	client := &agentx.Client{
		Net:               cfg.Prot,
		Address:           cfg.Host + ":" + cfg.Port,
		Timeout:           time.Duration(cfg.Timeout) * time.Minute,
		ReconnectInterval: time.Duration(cfg.Retry.Period) * time.Second,
	}

	if err := client.Open(); err != nil {
		return nil, err
	}

	return client.Session()
}

func healthServerInit(cfg *agentxConf) {
	s := &http.Server{
		Handler: http.HandlerFunc(healthRequestHandler)}
	l, err := net.Listen("tcp4", "0.0.0.0:"+cfg.Health.Port)
	if err != nil {
		log.Fatalf(errgo.Details(err))
	}
	go s.Serve(l.(*net.TCPListener))
}

func healthRequestHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "alive\n")
}

func pollingLoop(ctx *globalCtx, cfg *agentxConf) {
	for _, item := range cfg.Discovery.Items {
		ctx.wg.Add(1)
		go startPoll(pollerCtx{*ctx, item})
	}
	ctx.wg.Wait()
}

func startPoll(ctx pollerCtx) {
	/* log.Println("startPoll: begin goroutine") */
	defer ctx.wg.Done()

	baseIndexOID := ctx.baseOID + ctx.item.OID.Index + "."
	baseValueOID := ctx.baseOID + ctx.item.OID.Value + "."

	pollPeriod := ctx.item.Poll.Period
	handler := ctx.session.Handler.(*agentx.ListHandler)

	for {
		for result := range getValues(ctx.done, ctx.item, getIndexs(ctx.done, ctx.item)) {
			/* log.Println("startPoll: recieved pipeRows") */
			indexOid := baseIndexOID + result.Index
			valueOid := baseValueOID + result.Index
			ii := handler.Add(indexOid)
			ii.Type = pdu.VariableTypeInteger
			i64, _ := strconv.ParseInt(result.Index, 10, 32)
			ii.Value = int32(i64)
			vi := handler.Add(valueOid)
			vi.Type = pdu.VariableTypeOctetString
			vi.Value = result.Value
		}

		ctx.done <- true
		time.Sleep(time.Duration(pollPeriod) * time.Second)
	}
}

var getValues = func(done <-chan bool, cfg Item, indexStream <-chan string) <-chan cacheRow {
	cacheRowStream := make(chan cacheRow)

	go func() {
		/* defer log.Println("readIndexPipe exited!") */
		defer close(cacheRowStream)

		for _, i := range strings.Split(<-indexStream, " ") {

			if strings.Compare(i, "") == 0 {
				continue
			}

			args := append(cfg.Exec.Value.Args, i)
			cmd := exec.Command(cfg.Exec.Value.Cmd, args...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				log.Println(errgo.Details(err))
				return
			}

			if err = cmd.Start(); err != nil {
				log.Println(errgo.Details(err))
				return
			}

			defer stdout.Close()
			defer cmd.Process.Kill()

			timeout := time.Duration(cfg.Poll.Timeout) * time.Second
			ch := getPipeOutputStr(stdout)

			select {
			case <-done:
				return

			case <-time.After(timeout):
				/* log.Println("getValues: process were killed") */
				return

			case result := <-ch:
				select {
				case <-done:
					return

				case <-time.After(timeout):
					log.Println("getValues: process were killed")
					return

				case cacheRowStream <- cacheRow{i, result}:
					log.Printf("getValues: send to rowStream %#v %s", i, result)
					stdout.Close()
					cmd.Process.Kill()
				}

				if err = cmd.Wait(); err != nil {
					log.Println(errgo.Details(err))
				}
			}
		}
	}()

	return cacheRowStream
}

var getIndexs = func(done chan bool, cfg Item) <-chan string {
	indexStream := make(chan string)

	go func() {
		/* defer log.Println("getIndexs exited!") */
		defer close(indexStream)

		cmd := exec.Command(cfg.Exec.Index.Cmd, cfg.Exec.Index.Args...)
		stdout, err := cmd.StdoutPipe()

		if err = cmd.Start(); err != nil {
			log.Println(errgo.Details(err))
			return
		}

		defer stdout.Close()
		defer cmd.Process.Kill()

		timeout := time.Duration(cfg.Poll.Timeout) * time.Second
		ch := getPipeOutputStr(stdout)

		for {
			/* log.Println("getIndexs: output loop begin") */

			select {
			case <-done:
				return

			case <-time.After(timeout):
				log.Println("getIndexs: process were killed")
				return

			case strOfIndexs := <-ch:
				select {
				case <-done:
					return

				case <-time.After(timeout):
					log.Println("getIndexs: process were killed")
					return

				case indexStream <- strOfIndexs:
					log.Println("getIndexs: send to indexStream")
				}

				if err = cmd.Wait(); err != nil {
					log.Println(errgo.Details(err))
				}
			}
		}
	}()

	return indexStream
}

var getPipeOutputStr = func(stdout io.ReadCloser) chan string {
	outputStrPipe := make(chan string)

	go func() {
		/* defer log.Println("getPipeOutputStr: exited!") */
		defer close(outputStrPipe)
		b, _ := ioutil.ReadAll(stdout)
		outputStrPipe <- string(b)
	}()

	return outputStrPipe
}
