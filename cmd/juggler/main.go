package main

import (
	"context"
	"log"

	"github.com/cocaine/cocaine-framework-go/cocaine"
	"github.com/combaine/combaine/common"
	"github.com/combaine/combaine/common/logger"
	"github.com/combaine/combaine/senders/juggler"
)

type senderTask struct {
	ID     string `codec:"Id"`
	Data   []common.AggregationResult
	Config juggler.Config
}

var senderConfig *juggler.SenderConfig

func send(request *cocaine.Request, response *cocaine.Response) {
	defer response.Close()

	raw := <-request.Read()
	var task senderTask
	err := common.Unpack(raw, &task)
	if err != nil {
		logger.Errf("%s Failed to unpack juggler task %s", task.ID, err)
		return
	}
	// common.Unpack unpack some strings as []byte, need convert it
	juggler.StringifyAggregatorLimits(task.Config.AggregatorKWArgs.Limits)
	task.Config.Tags = juggler.EnsureDefaultTag(task.Config.Tags)

	err = juggler.UpdateTaskConfig(&task.Config, senderConfig)
	if err != nil {
		logger.Errf("%s Failed to update task config %s", task.ID, err)
		return
	}
	logger.Debugf("%s Task: %v", task.ID, task)

	jCli, err := juggler.NewSender(&task.Config, task.ID)
	if err != nil {
		logger.Errf("%s Unexpected error %s", task.ID, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), juggler.DefaultTimeout)
	defer cancel()
	err = jCli.Send(ctx, task.Data)
	if err != nil {
		logger.Errf("%s Sending error %s", task.ID, err)
		return
	}
	response.Write("DONE")
}

func main() {
	var err error
	senderConfig, err = juggler.GetSenderConfig()
	if err != nil {
		log.Fatalf("Failed to load sender config %s", err)
	}

	juggler.InitializeLogger(logger.MustCreateLogger)

	juggler.GlobalCache.TuneCache(senderConfig.CacheTTL, senderConfig.CacheCleanInterval)
	juggler.InitEventsStore(&senderConfig.Store)

	binds := map[string]cocaine.EventHandler{
		"send": send,
	}
	Worker, err := cocaine.NewWorker()
	if err != nil {
		log.Fatal(err)
	}
	Worker.Loop(binds)
}
