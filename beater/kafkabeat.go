package beater

import (
	"fmt"
	"time"
	"strconv"
	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/cfgfile"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/wvanbergen/kazoo-go"
	"github.com/Shopify/sarama"
	"github.com/gingerwizard/kafkabeat/config"
)

var client sarama.Client
var zClient *kazoo.Kazoo

type KafkabeatError struct {
	error string
}

type Kafkabeat struct {
	beatConfig *config.Config
	done       chan struct{}
	period     time.Duration

	topics     [] string
	groups     [] string
	zookeepers    [] string
	brokers [] string
}

// Creates beater
func New() *Kafkabeat {
	return &Kafkabeat{
		done: make(chan struct{}),
	}
}

/// *** Beater interface methods ***///

func (bt *Kafkabeat) Config(b *beat.Beat) error {

	// Load beater beatConfig
	logp.Info("Configuring Kafkabeat...")
	var err error
	err = cfgfile.Read(&bt.beatConfig, "")
	if err != nil {
		return fmt.Errorf("Error reading config file: %v", err)
	}

	bt.zookeepers = bt.beatConfig.Kafkabeat.Zookeepers
	if bt.zookeepers == nil || len(bt.zookeepers) == 0 {
		return KafkabeatError{"Atleast one zookeeper must be defined"}
	}
	zClient,err = kazoo.NewKazoo(bt.zookeepers,nil)
	if err != nil {
		logp.Err("Unable to connect to Zookeeper")
		return err
	}
	bt.brokers,err = zClient.BrokerList()
	if err != nil{
		logp.Err("Error identifying brokers from zookeeper")
		return err
	}
	if (bt.brokers == nil || len(bt.brokers) == 0) {
		return KafkabeatError{"Unable to identify active brokers"}
	}
	logp.Info("Brokers: %v",bt.brokers)
	client,err = sarama.NewClient(bt.brokers,sarama.NewConfig())

	// Setting default period if not set
	if bt.beatConfig.Kafkabeat.Period == "" {
		bt.beatConfig.Kafkabeat.Period = "1s"
	}

	bt.period, err = time.ParseDuration(bt.beatConfig.Kafkabeat.Period)
	if err != nil {
		return err
	}
	bt.topics = bt.beatConfig.Kafkabeat.Topics
	if bt.topics == nil || len(bt.topics) == 0 {
		bt.topics,err = client.Topics()
	}
	if err != nil {
		return err
	}
	logp.Info("Monitoring topics: %v",bt.topics)
	bt.groups = bt.beatConfig.Kafkabeat.Groups
	if bt.groups == nil {
		bt.groups,err = getGroups()
	}
	logp.Info("Monitoring groups %v",bt.groups)
	return err
}


func getGroups() ([]string,error) {
	group_list,err :=zClient.Consumergroups()
	if err != nil {
		logp.Err("Unable to retrieve groups")
		return nil,err
	}
	groups:=make([]string,len(group_list))
	for i, group := range group_list {
		groups[i]=group.Name
	}
	return groups,nil
}


func (bt *Kafkabeat) Setup(b *beat.Beat) error {
	return nil
}

func (bt *Kafkabeat) Run(b *beat.Beat) error {
	logp.Info("kafkabeat is running! Hit CTRL-C to stop it.")
	ticker := time.NewTicker(bt.period)
	for {
		select {
		case <-bt.done:
			return nil
		case <-ticker.C:
			for _, topic := range bt.topics {
				pid_sizes,err := processTopic(topic,b)
				if err == nil {
					events:=processGroups(bt.groups,topic,pid_sizes)
					if events !=nil && len(events) > 0 {
						b.Events.PublishEvents(events)
					}
				}
			}
		}
	}
}

func processTopic(topic string,b *beat.Beat) (map[int32]int64,error){
	pids, err := client.Partitions(topic)
	if err != nil {
		logp.Err("Unable to retrieve paritions for topic %v",topic)
		return nil,err
	}
	logp.Info("Partitions retrieved for topic %v",topic)
	pid_sizes := getPartitionSizes(topic, pids)
	events:=make([]common.MapStr,len(pid_sizes))
	counter:=0
	for pid,size := range pid_sizes {
		events[counter] = common.MapStr{
			"@timestamp": common.Time(time.Now()),
			"type": "topic",
			"partition": pid,
			"topic":topic,
			"logSize": size,
		}
		counter++
	}
	if len(events) > 0 {
		b.Events.PublishEvents(events)
		logp.Info("%v Events sent",len(events))
	}
	return pid_sizes,nil
}


func processGroups(groups []string, topic string,pids map[int32]int64) ([]common.MapStr){
	var events []common.MapStr
	for _,group := range groups {
		pid_offsets,err := getConsumerOffsets(group, topic, pids)
		if err == nil {
			for pid,offset := range pid_offsets {
				event:=common.MapStr{
					"@timestamp": common.Time(time.Now()),
					"type": "consumer",
					"partition": pid,
					"topic":topic,
					"group": group,
					"offset": offset,
				}
				size,ok := pids[pid]
				if ok {
					event.Update(common.MapStr{"lag":size-offset,})
				}
				events=append(events,event)
			}
		} else {
			logp.Debug("kafkabeat","No offsets for group %s on topic %s", group, topic)
		}
	}
	return events
}


func getPartitionSizes(topic string, pids []int32) (map[int32]int64){
	pId_sizes := make(map[int32]int64)
	for _, pid := range pids {
		logp.Debug("kafkabeat","Processing partition %v", pid)
		pid_size,err:=client.GetOffset(topic, pid,sarama.OffsetNewest)
		if err != nil {
			logp.Err("Unable to identify size for partition %s and topic %s", pid,topic)
		} else {
			logp.Debug("kafkabeat","Current log size is %v for partition %v", strconv.FormatInt(pid_size,10), pid)
			pId_sizes[pid]=pid_size
		}

	}
	return pId_sizes
}


func getConsumerOffsets(group string, topic string, pids map[int32]int64) (map[int32]int64,error) {
	broker,err := client.Coordinator(group)
	offsets := make(map[int32]int64)
	if err != nil {
		logp.Err("Unable to identify group coordinator for group %v",group)
	} else {
		request:=sarama.OffsetFetchRequest{ConsumerGroup:group,Version:0}
		for pid, size := range pids {
			if size > 0 {
				request.AddPartition(topic, pid)
			}
		}
		res,err := broker.FetchOffset(&request)
		if err != nil {
			logp.Err("Issue fetching offsets coordinator for topic %v",topic)
			logp.Err("%v",err)
		}
		if res != nil {
			for pid,_  := range pids {
				offset := res.GetBlock(topic, pid)
				if offset != nil && offset.Offset > -1{
					offsets[pid]=offset.Offset
				}
			}
		}
	}
	return offsets,err
}



func (bt *Kafkabeat) Cleanup(b *beat.Beat) error {
	return client.Close()
}

func (bt *Kafkabeat) Stop() {
	close(bt.done)
}

func (err KafkabeatError) Error() string {
	return err.error
}