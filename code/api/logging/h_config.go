package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	lapi "server/code/api"
	"server/code/client"
	"time"

	"github.com/jkstack/anet"
	"github.com/lwch/api"
	"github.com/lwch/logging"
	"github.com/lwch/runtime"
)

type configArgs struct {
	Exclude  string      `json:"exclude"`
	Batch    int         `json:"batch"`
	Buffer   int         `json:"buffer"`
	Interval int         `json:"interval"`
	K8s      *k8sConfig  `json:"k8s,omitempty"`
	File     *fileConfig `json:"file,omitempty"`
}

type context struct {
	ID      int64      `json:"id"`
	Args    configArgs `json:"args"`
	CID     string     `json:"cid"`
	Started bool       `json:"started"`
}

func (h *Handler) config(clients *client.Clients, ctx *api.Context) {
	t := ctx.XStr("type")
	var rt context
	rt.ID = ctx.XInt64("pid")
	rt.Args.Exclude = ctx.OStr("exclude", "")
	rt.Args.Batch = ctx.OInt("batch", 1000)
	rt.Args.Buffer = ctx.OInt("buffer", 4096)
	rt.Args.Interval = ctx.OInt("interval", 30)

	var err error

	if len(rt.Args.Exclude) > 0 {
		_, err = regexp.Compile(rt.Args.Exclude)
		if err != nil {
			lapi.BadParamErr(fmt.Sprintf("exclude: %v", err))
			return
		}
	}

	switch t {
	case "k8s":
		rt.Args.K8s = new(k8sConfig)
		err = rt.Args.K8s.build(ctx)
	case "docker":
		err = errors.New("unsupported")
	case "logtail":
		rt.Args.File = new(fileConfig)
		err = rt.Args.File.build(ctx)
	default:
		lapi.BadParamErr("type")
		return
	}
	runtime.Assert(err)

	rt.CID, err = rt.Args.send(clients, rt.ID, h.cfg.LoggingReport)
	if err == errNoCollector {
		ctx.ERR(1, err.Error())
		return
	}
	runtime.Assert(err)

	dir := filepath.Join(h.cfg.DataDir, "logging", fmt.Sprintf("%d.json", rt.ID))
	err = saveConfig(dir, rt)
	runtime.Assert(err)

	h.Lock()
	h.data[rt.ID] = &rt
	h.Unlock()

	ctx.OK(nil)
}

func saveConfig(dir string, rt context) error {
	os.MkdirAll(filepath.Dir(dir), 0755)
	f, err := os.Create(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rt)
}

func (ctx *context) reSend(cli *client.Client, report string) {
	err := ctx.Args.sendTo(cli, ctx.ID, report)
	if err != nil {
		logging.Error("send logging config of project %d to client [%s]: %v",
			ctx.ID, cli.ID())
		return
	}
	if ctx.Started {
		taskID, err := cli.SendLoggingStart(ctx.ID)
		if err != nil {
			logging.Error("send logging start of project %d: %v",
				ctx.ID, cli.ID())
			return
		}
		defer cli.ChanClose(taskID)
		var msg *anet.Msg
		select {
		case msg = <-cli.ChanRead(taskID):
		case <-time.After(api.RequestTimeout):
			logging.Error("wait logging start status of project %d: %v",
				ctx.ID, cli.ID())
			return
		}

		switch {
		case msg.Type == anet.TypeError:
			logging.Error("get logging start status of project %d: %v",
				ctx.ID, cli.ID())
			return
		case msg.Type != anet.TypeLoggingStatusRep:
			logging.Error("get logging start status of project %d: %v",
				ctx.ID, cli.ID())
			return
		}

		if !msg.LoggingStatusRep.OK {
			logging.Error("get logging start status of project %d: %v",
				ctx.ID, cli.ID())
			return
		}
	}
}

func (args *configArgs) sendTo(cli *client.Client, pid int64, report string) error {
	switch {
	case args.K8s != nil:
		_, err := cli.SendLoggingConfigK8s(pid, args.Exclude,
			args.Batch, args.Buffer, args.Interval, report,
			args.K8s.Namespace, args.K8s.Names, args.K8s.Dir, args.K8s.Api, args.K8s.Token)
		return err
	case args.File != nil:
		_, err := cli.SendLoggingConfigFile(pid, args.Exclude,
			args.Batch, args.Buffer, args.Interval, report,
			args.File.Dir)
		return err
	default:
		return errors.New("unsupported")
	}
}

func (args *configArgs) send(clients *client.Clients, pid int64, report string) (string, error) {
	var cli *client.Client
	switch {
	case args.K8s != nil:
		clis := clients.Prefix(args.K8s.Namespace + "-k8s-")
		if len(clis) == 0 {
			clis = clients.Prefix("k8s-")
			if len(clis) == 0 {
				return "", errNoCollector
			}
		}
		cli = clis[int(pid)%len(clis)]
		err := args.sendTo(cli, pid, report)
		if err != nil {
			return "", err
		}
		return cli.ID(), nil
	case args.File != nil:
		for _, cli := range clients.All() {
			err := args.sendTo(cli, pid, report)
			if err != nil {
				logging.Error("broadcast file logging config to %s: %v", cli.ID(), err)
				continue
			}
		}
		return "", nil
	default:
		return "", errors.New("unsupported")
	}
}
