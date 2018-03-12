package main

/*
* daemonization:
* - is a OS level question, not a programming language level question
* - using init systems like systemd, launchd, daemontools, supervisor,
*   runit, Kubernetes, heroku, Borg, etc etc
* - https://github.com/sevlyar/go-daemon
 */

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
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
	confFile = "./agentx.yml"
)

func main() {

	cfg := agentxConf{}
	fn, _ := filepath.Abs(confFile)

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

	ctx := globalCtx{
		session: session,
		baseOID: cfg.Discovery.OID}

	if err := setSignal(&ctx); err != nil {
		log.Fatalf(errgo.Details(err))
	}

	pollingLoop(&ctx, &cfg)
}

func setSignal(ctx *globalCtx) error {
	signal_chan := make(chan os.Signal, 1)
	signal.Notify(signal_chan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	exit_chan := make(chan int)
	go func() {
		for {
			s := <-signal_chan
			switch s {
			// kill -SIGHUP XXXX
			case syscall.SIGHUP:
				fmt.Println("hungup")

				// kill -SIGINT XXXX or Ctrl+c
			case syscall.SIGINT:
				fmt.Println("Warikomi")

				// kill -SIGTERM XXXX
			case syscall.SIGTERM:
				fmt.Println("force stop")
				exit_chan <- 0

				// kill -SIGQUIT XXXX
			case syscall.SIGQUIT:
				fmt.Println("stop and core dump")
				exit_chan <- 0

			default:
				fmt.Println("Unknown signal.")
				exit_chan <- 1
			}
		}
		appCleanup(ctx)
		code := <-exit_chan
		os.Exit(code)
	}()
	return nil
}

func appCleanup(ctx *globalCtx) {
	log.Println("CLEANUP APP BEFORE EXIT!!!")
	ctx.done <- true
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

func pollingLoop(ctx *globalCtx, cfg *agentxConf) {
	for _, item := range cfg.Discovery.Items {
		ctx.wg.Add(1)
		go startPoll(pollerCtx{*ctx, item})
	}
	ctx.wg.Wait()
}

func startPoll(ctx pollerCtx) {
	done := make(chan bool)

	log.Println("startPoll: begin goroutine")

	defer close(done)
	defer ctx.wg.Done()

	baseIndexOID := ctx.baseOID + ctx.item.OID.Index + "."
	baseValueOID := ctx.baseOID + ctx.item.OID.Value + "."

	pollPeriod := ctx.item.Poll.Period
	handler := ctx.session.Handler.(*agentx.ListHandler)

	for {
		for result := range getValues(done, ctx.item, getIndexs(done, ctx.item)) {
			log.Println("startPoll: recieved pipeRows")
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

		done <- true
		time.Sleep(time.Duration(pollPeriod) * time.Second)
	}
}

var getValues = func(done <-chan bool, cfg Item, indexStream <-chan string) <-chan cacheRow {
	cacheRowStream := make(chan cacheRow)

	go func() {
		defer log.Println("readIndexPipe exited!")
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
				log.Println("getValues: process were killed")
				return

			case result := <-ch:
				select {
				case <-done:
					return

				case <-time.After(timeout):
					log.Println("getValues: process were killed")
					return

				case cacheRowStream <- cacheRow{i, result}:
					log.Printf("getValues: send to rowStream %#v", i)
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
		defer log.Println("getIndexs exited!")
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
			log.Println("getIndexs: output loop begin")

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
		defer log.Println("getPipeOutputStr: exited!")
		defer close(outputStrPipe)
		b, _ := ioutil.ReadAll(stdout)
		outputStrPipe <- string(b)
	}()

	return outputStrPipe
}
