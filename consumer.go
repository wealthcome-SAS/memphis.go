// Credit for The NATS.IO Authors
// Copyright 2021-2022 The Memphis Authors
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.package server

package memphis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	consumerDefaultPingInterval    = 30 * time.Second
	dlsSubjPrefix                  = "$memphis_dls"
	memphisPmAckSubject            = "$memphis_pm_acks"
	lastConsumerCreationReqVersion = 4
	lastConsumerDestroyReqVersion  = 1
)

var (
	ConsumerErrStationUnreachable = errors.New("station unreachable")
	ConsumerErrConsumeInactive    = errors.New("consumer is inactive")
	ConsumerErrDelayDlsMsg        = errors.New("cannot delay DLS message")
)

// Consumer - memphis consumer object.
type Consumer struct {
	Name                     string
	ConsumerGroup            string
	PullInterval             time.Duration
	BatchSize                int
	BatchMaxTimeToWait       time.Duration
	MaxAckTime               time.Duration
	MaxMsgDeliveries         int
	conn                     *Conn
	stationName              string
	jsConsumers              map[int]jetstream.Consumer
	pingInterval             time.Duration
	subscriptionActive       bool
	consumeActive            bool
	consumeQuit              chan struct{}
	pingQuit                 chan struct{}
	errHandler               ConsumerErrHandler
	StartConsumeFromSequence uint64
	LastMessages             int64
	context                  context.Context
	realName                 string
	dlsCurrentIndex          int
	dlsHandlerFunc           ConsumeHandler
	dlsMsgs                  []*Msg
	dlsMsgsMutex             sync.RWMutex
	PartitionGenerator       *RoundRobinProducerConsumerGenerator
}

// Msg - a received message, can be acked.
type Msg struct {
	msg                 any
	conn                *Conn
	cgName              string
	internalStationName string
}

type PMsgToAck struct {
	ID     int    `json:"id"`
	CgName string `json:"cg_name"`
}

// Msg.Data - get message's data.
func (m *Msg) Data() []byte {
	if msg, ok := m.msg.(*nats.Msg); ok {
		return msg.Data
	} else {
		if jsMsg, ok := m.msg.(jetstream.Msg); ok {
			return jsMsg.Data()
		}
	}
	return nil
}

// Msg.DataDeserialized - get message's deserialized data.
func (m *Msg) DataDeserialized() (any, error) {
	var data map[string]interface{}

	sd, err := m.conn.getSchemaDetails(m.internalStationName)
	if err != nil {
		return nil, memphisError(errors.New("Schema validation has failed: " + err.Error()))
	}
	var msgBytes []byte

	if msg, ok := m.msg.(*nats.Msg); ok {
		msgBytes = msg.Data
	} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
		msgBytes = jsMsg.Data()
	} else {
		return nil, errors.New("Message format is not supported")
	}

	_, err = sd.validateMsg(msgBytes)
	if err != nil {
		return nil, memphisError(errors.New("Deserialization has been failed since the message format does not align with the currently attached schema: " + err.Error()))
	}

	switch sd.schemaType {
	case "protobuf":
		pMsg := dynamicpb.NewMessage(sd.msgDescriptor)
		err = proto.Unmarshal(msgBytes, pMsg)
		if err != nil {
			if strings.Contains(err.Error(), "cannot parse invalid wire-format data") {
				err = errors.New("invalid message format, expecting protobuf")
			}
			return data, memphisError(err)
		}
		jsonBytes, err := protojson.Marshal(pMsg)
		if err != nil {
			panic(err)
		}
		if err := json.Unmarshal(jsonBytes, &data); err != nil {
			err = errors.New("Bad JSON format - " + err.Error())
			return data, memphisError(err)
		}
		return data, nil
	case "json":
		if err := json.Unmarshal(msgBytes, &data); err != nil {
			err = errors.New("Bad JSON format - " + err.Error())
			return data, memphisError(err)
		}
		return data, nil
	case "graphql":
		return string(msgBytes), nil
	case "avro":
		if err := json.Unmarshal(msgBytes, &data); err != nil {
			err = errors.New("Bad JSON format - " + err.Error())
			return data, memphisError(err)
		}
		return data, nil
	default:
		return msgBytes, nil
	}
}

// Msg.GetSequenceNumber - get message's sequence number
func (m *Msg) GetSequenceNumber() (uint64, error) {
	var seq uint64

	if msg, ok := m.msg.(*nats.Msg); ok {
		meta, err := msg.Metadata()
		if err != nil {
			return 0, nil
		}
		seq = meta.Sequence.Stream
	} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
		meta, err := jsMsg.Metadata()
		if err != nil {
			return 0, nil
		}
		seq = meta.Sequence.Stream
	} else {
		return 0, errors.New("message format is not supported")
	}

	return seq, nil
}

// Msg.Ack - ack the message.
func (m *Msg) Ack() error {
	var err error
	if msg, ok := m.msg.(*nats.Msg); ok {
		err = msg.Ack()
	} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
		err = jsMsg.Ack()
	} else {
		return errors.New("Message format is not supported")
	}
	if err != nil {
		var headers nats.Header
		if msg, ok := m.msg.(*nats.Msg); ok {
			headers = msg.Header
		} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
			headers = jsMsg.Headers()
		}
		id, ok := headers["$memphis_pm_id"]
		if !ok {
			return err
		} else {
			idNumber, err := strconv.Atoi(id[0])
			if err != nil {
				return err
			}
			cgName, ok := headers["$memphis_pm_cg_name"]
			if !ok {
				return err
			} else {
				msgToAck := PMsgToAck{
					ID:     idNumber,
					CgName: cgName[0],
				}
				msgToPublish, _ := json.Marshal(msgToAck)
				m.conn.brokerConn.Publish(memphisPmAckSubject, msgToPublish)
			}
		}
	}
	return nil
}

// Msg.GetHeaders - get headers per message
func (m *Msg) GetHeaders() map[string]string {
	headers := map[string]string{}
	var natsHeaders nats.Header
	if msg, ok := m.msg.(*nats.Msg); ok {
		natsHeaders = msg.Header
	} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
		natsHeaders = jsMsg.Headers()
	} else {
		return headers
	}
	for key, value := range natsHeaders {
		if strings.HasPrefix(key, "$memphis") {
			continue
		}
		headers[key] = value[0]
	}
	return headers
}

// Msg.Delay - Delay a message redelivery
func (m *Msg) Delay(duration time.Duration) error {
	headers := m.GetHeaders()
	_, pmOk := headers["$memphis_pm_id"]
	_, cgOk := headers["$memphis_pm_cg_name"]
	if !pmOk || !cgOk {
		if msg, ok := m.msg.(*nats.Msg); ok {
			return msg.NakWithDelay(duration)
		} else if jsMsg, ok := m.msg.(jetstream.Msg); ok {
			return jsMsg.NakWithDelay(duration)
		} else {
			return errors.New("Message format is not supported")
		}
	}
	return memphisError(ConsumerErrDelayDlsMsg)
}

// ConsumerErrHandler is used to process asynchronous errors.
type ConsumerErrHandler func(*Consumer, error)

type createConsumerReq struct {
	Name                     string `json:"name"`
	StationName              string `json:"station_name"`
	ConnectionId             string `json:"connection_id"`
	ConsumerType             string `json:"consumer_type"`
	ConsumerGroup            string `json:"consumers_group"`
	MaxAckTimeMillis         int    `json:"max_ack_time_ms"`
	MaxMsgDeliveries         int    `json:"max_msg_deliveries"`
	Username                 string `json:"username"`
	StartConsumeFromSequence uint64 `json:"start_consume_from_sequence"`
	LastMessages             int64  `json:"last_messages"`
	RequestVersion           int    `json:"req_version"`
	AppId                    string `json:"app_id"`
	SdkLang                  string `json:"sdk_lang"`
}

type removeConsumerReq struct {
	Name           string `json:"name"`
	StationName    string `json:"station_name"`
	Username       string `json:"username"`
	ConnectionId   string `json:"connection_id"`
	RequestVersion int    `json:"req_version"`
}

// ConsumerOpts - configuration options for a consumer.
type ConsumerOpts struct {
	Name                     string
	StationName              string
	ConsumerGroup            string
	PullInterval             time.Duration
	BatchSize                int
	BatchMaxTimeToWait       time.Duration
	MaxAckTime               time.Duration
	MaxMsgDeliveries         int
	GenUniqueSuffix          bool
	ErrHandler               ConsumerErrHandler
	StartConsumeFromSequence uint64
	LastMessages             int64
	TimeoutRetry             int
}

type createConsumerResp struct {
	SchemaUpdateInit SchemaUpdateInit `json:"schema_update"`
	PartitionsUpdate PartitionsUpdate `json:"partitions_update"`
	Err              string           `json:"error"`
}

// getDefaultConsumerOptions - returns default configuration options for consumers.
func getDefaultConsumerOptions() ConsumerOpts {
	return ConsumerOpts{
		PullInterval:             1 * time.Second,
		BatchSize:                10,
		BatchMaxTimeToWait:       5 * time.Second,
		MaxAckTime:               30 * time.Second,
		MaxMsgDeliveries:         2,
		GenUniqueSuffix:          false,
		ErrHandler:               DefaultConsumerErrHandler,
		StartConsumeFromSequence: 1,
		LastMessages:             -1,
		TimeoutRetry:             5,
	}
}

// ConsumerOpt  - a function on the options for consumers.
type ConsumerOpt func(*ConsumerOpts) error

// CreateConsumer - creates a consumer.
func (c *Conn) CreateConsumer(stationName, consumerName string, opts ...ConsumerOpt) (*Consumer, error) {
	defaultOpts := getDefaultConsumerOptions()

	defaultOpts.Name = consumerName
	defaultOpts.StationName = stationName
	for _, opt := range opts {
		if opt != nil {
			if err := opt(&defaultOpts); err != nil {
				return nil, memphisError(err)
			}
		}
	}
	if defaultOpts.ConsumerGroup == "" {
		defaultOpts.ConsumerGroup = consumerName
	}
	consumer, err := defaultOpts.createConsumer(c, TimeoutRetry(defaultOpts.TimeoutRetry))
	if err != nil {
		return nil, memphisError(err)
	}
	c.cacheConsumer(consumer)

	return consumer, nil
}

// ConsumerOpts.createConsumer - creates a consumer using a configuration struct.
func (opts *ConsumerOpts) createConsumer(c *Conn, options ...RequestOpt) (*Consumer, error) {
	var err error
	name := strings.ToLower(opts.Name)
	nameWithoutSuffix := name
	if opts.GenUniqueSuffix {
		opts.Name, err = extendNameWithRandSuffix(opts.Name)
		if err != nil {
			return nil, memphisError(err)
		}
	}

	consumer := Consumer{Name: opts.Name,
		ConsumerGroup:            opts.ConsumerGroup,
		PullInterval:             opts.PullInterval,
		BatchSize:                opts.BatchSize,
		MaxAckTime:               opts.MaxAckTime,
		MaxMsgDeliveries:         opts.MaxMsgDeliveries,
		BatchMaxTimeToWait:       opts.BatchMaxTimeToWait,
		conn:                     c,
		stationName:              opts.StationName,
		errHandler:               opts.ErrHandler,
		StartConsumeFromSequence: opts.StartConsumeFromSequence,
		LastMessages:             opts.LastMessages,
		dlsMsgs:                  []*Msg{},
		dlsCurrentIndex:          0,
		dlsHandlerFunc:           nil,
		realName:                 nameWithoutSuffix,
	}

	if consumer.StartConsumeFromSequence == 0 {
		return nil, memphisError(errors.New("startConsumeFromSequence has to be a positive number"))
	}

	if consumer.LastMessages < -1 {
		return nil, memphisError(errors.New("min value for LastMessages is -1"))
	}

	if consumer.StartConsumeFromSequence > 1 && consumer.LastMessages > -1 {
		return nil, memphisError(errors.New("Consumer creation options can't contain both startConsumeFromSequence and lastMessages"))
	}

	if consumer.BatchSize > maxBatchSize || consumer.BatchSize < 1 {
		return nil, memphisError(errors.New("Batch size can not be greater than " + strconv.Itoa(maxBatchSize) + " or less than 1"))
	}

	sn := getInternalName(consumer.stationName)
	_, ok := c.stationUpdatesSubs[sn]
	if !ok {
		c.stationUpdatesSubs[sn] = &stationUpdateSub{
			refCount:       1,
			schemaUpdateCh: make(chan SchemaUpdate),
			schemaDetails:  schemaDetails{},
		}
	}

	err = c.create(&consumer, options...)
	if err != nil {
		return nil, memphisError(err)
	}

	consumer.consumeQuit = make(chan struct{})
	consumer.pingQuit = make(chan struct{}, 1)

	consumer.pingInterval = consumerDefaultPingInterval

	err = c.listenToSchemaUpdates(opts.StationName)
	if err != nil {
		return nil, memphisError(err)
	}

	durable := getInternalName(consumer.ConsumerGroup)

	if len(consumer.conn.stationPartitions[sn].PartitionsList) == 0 {
		consumer.jsConsumers = make(map[int]jetstream.Consumer, 1)
		jsCons, err := c.jetstreamConsumer(sn, durable)
		if err != nil {
			return nil, memphisError(err)
		}
		consumer.jsConsumers[1] = jsCons
	} else {
		consumer.jsConsumers = make(map[int]jetstream.Consumer, len(consumer.conn.stationPartitions[sn].PartitionsList))
		for _, p := range consumer.conn.stationPartitions[sn].PartitionsList {
			streamName := fmt.Sprintf("%s$%s", sn, strconv.Itoa(p))
			jsCons, err := c.jetstreamConsumer(streamName, durable)
			if err != nil {
				return nil, memphisError(err)
			}
			consumer.jsConsumers[p] = jsCons
		}
	}

	consumer.subscriptionActive = true

	go consumer.pingConsumer()
	err = consumer.dlsSubscriptionInit()
	if err != nil {
		return nil, memphisError(err)
	}
	c.cacheConsumer(&consumer)

	return &consumer, err
}

// Station.CreateConsumer - creates a producer attached to this station.
func (s *Station) CreateConsumer(name string, opts ...ConsumerOpt) (*Consumer, error) {
	return s.conn.CreateConsumer(s.Name, name, opts...)
}

func DefaultConsumerErrHandler(c *Consumer, err error) {
	log.Printf("Consumer %v: %v", c.Name, memphisError(err).Error())
}

func (c *Consumer) callErrHandler(err error) {
	if c.errHandler != nil {
		c.errHandler(c, err)
	}
}

func (c *Consumer) pingConsumer() {
	ticker := time.NewTicker(c.pingInterval)
	if !c.subscriptionActive {
		log.Fatal("started ping for inactive subscription")
	}

	for {
		select {
		case <-ticker.C:
			var generalErr error
			wg := sync.WaitGroup{}
			wg.Add(len(c.jsConsumers))
			for _, jscons := range c.jsConsumers {
				go func(jscons jetstream.Consumer) {
					ctx, cancelfunc := context.WithTimeout(context.Background(), JetstreamOperationTimeout*time.Second)
					defer cancelfunc()
					_, err := jscons.Info(ctx)
					if err != nil {
						generalErr = err
						wg.Done()
						return
					}
					wg.Done()
				}(jscons)
			}
			wg.Wait()
			if generalErr != nil {
				if strings.Contains(generalErr.Error(), "consumer not found") || strings.Contains(generalErr.Error(), "stream not found") {
					c.subscriptionActive = false
					c.callErrHandler(ConsumerErrStationUnreachable)
				}
			}
		case <-c.pingQuit:
			ticker.Stop()
			return
		}
	}
}

// Consumer.SetContext - set a context that will be passed to each message handler function call
func (c *Consumer) SetContext(ctx context.Context) {
	c.context = ctx
}

// ConsumeHandler - handler for consumed messages
type ConsumeHandler func([]*Msg, error, context.Context)

// ConsumingOpts - configuration options for consuming messages
type ConsumingOpts struct {
	ConsumerPartitionKey    string
	ConsumerPartitionNumber int
}

type ConsumingOpt func(*ConsumingOpts) error

// ConsumerPartitionKey - Partition key for the consumer to consume from
func ConsumerPartitionKey(ConsumerPartitionKey string) ConsumingOpt {
	return func(opts *ConsumingOpts) error {
		opts.ConsumerPartitionKey = ConsumerPartitionKey
		return nil
	}
}

// ConsumerPartitionNumber - Partition number for the consumer to consume from
func ConsumerPartitionNumber(ConsumerPartitionNumber int) ConsumingOpt {
	return func(opts *ConsumingOpts) error {
		opts.ConsumerPartitionNumber = ConsumerPartitionNumber
		return nil
	}
}

func getDefaultConsumingOptions() ConsumingOpts {
	return ConsumingOpts{
		ConsumerPartitionKey:    "",
		ConsumerPartitionNumber: -1,
	}
}

// Consumer.Consume - start consuming messages according to the interval configured in the consumer object.
// When a batch is consumed the handlerFunc will be called.
func (c *Consumer) Consume(handlerFunc ConsumeHandler, opts ...ConsumingOpt) error {

	defaultOpts := getDefaultConsumingOptions()

	for _, opt := range opts {
		if opt != nil {
			if err := opt(&defaultOpts); err != nil {
				return memphisError(err)
			}
		}
	}

	go func(c *Consumer, partitionKey string, partitionNumber int) {

		msgs, err := c.fetchSubscription(partitionKey, partitionNumber)
		handlerFunc(msgs, memphisError(err), c.context)
		c.dlsHandlerFunc = handlerFunc
		ticker := time.NewTicker(c.PullInterval)
		defer ticker.Stop()

		for {
			// give first priority to quit signals
			select {
			case <-c.consumeQuit:
				return
			default:
			}

			select {
			case <-ticker.C:
				msgs, err := c.fetchSubscription(partitionKey, partitionNumber)
				handlerFunc(msgs, memphisError(err), c.context)
			case <-c.consumeQuit:
				return
			}
		}
	}(c, defaultOpts.ConsumerPartitionKey, defaultOpts.ConsumerPartitionNumber)
	c.consumeActive = true
	return nil
}

// StopConsume - stops the continuous consume operation.
func (c *Consumer) StopConsume() {
	if !c.consumeActive {
		c.callErrHandler(ConsumerErrConsumeInactive)
		return
	}
	c.consumeQuit <- struct{}{}
	c.consumeActive = false
}

func (c *Consumer) fetchSubscription(partitionKey string, partitionNum int) ([]*Msg, error) {
	if !c.subscriptionActive {
		return nil, memphisError(errors.New("station unreachable"))
	}
	wrappedMsgs := make([]*Msg, 0, c.BatchSize)
	partitionNumber := 1

	if len(c.jsConsumers) > 1 {
		if partitionKey != "" && partitionNum > 0 {
			return nil, memphisError(fmt.Errorf("Can not use both partition number and partition key"))
		}
		if partitionKey != "" {
			partitionFromKey, err := c.conn.GetPartitionFromKey(partitionKey, c.stationName)
			if err != nil {
				return nil, memphisError(err)
			}
			partitionNumber = partitionFromKey
		} else if partitionNum > 0 {
			err := c.conn.ValidatePartitionNumber(partitionNum, c.stationName)
			if err != nil {
				return nil, memphisError(err)
			}
			partitionNumber = partitionNum
		} else {
			partitionNumber = c.PartitionGenerator.Next()
		}
	}

	batch, err := c.jsConsumers[partitionNumber].Fetch(c.BatchSize, jetstream.FetchMaxWait(c.BatchMaxTimeToWait))
	if err != nil && err != nats.ErrTimeout {
		c.subscriptionActive = false
		c.callErrHandler(ConsumerErrStationUnreachable)
		c.StopConsume()
	}
	if batch.Error() != nil && batch.Error() != nats.ErrTimeout {
		c.subscriptionActive = false
		c.callErrHandler(ConsumerErrStationUnreachable)
		c.StopConsume()
	}
	// msgs := batch.Messages()
	internalStationName := getInternalName(c.stationName)
	for msg := range batch.Messages() {
		wrappedMsgs = append(wrappedMsgs, &Msg{msg: msg, conn: c.conn, cgName: c.ConsumerGroup, internalStationName: internalStationName})
	}
	return wrappedMsgs, nil
}

type fetchResult struct {
	msgs []*Msg
	err  error
}

func (c *Consumer) fetchSubscriprionWithTimeout(partitionKey string, partitionNumber int) ([]*Msg, error) {
	timeoutDuration := c.BatchMaxTimeToWait
	out := make(chan fetchResult, 1)

	go func(partitionKey string) {
		msgs, err := c.fetchSubscription(partitionKey, partitionNumber)
		out <- fetchResult{msgs: msgs, err: memphisError(err)}
	}(partitionKey)
	select {
	case <-time.After(timeoutDuration):
		return []*Msg{}, nil
	case fetchRes := <-out:
		return fetchRes.msgs, memphisError(fetchRes.err)
	}
}

// Fetch - immediately fetch a batch of messages.
func (c *Consumer) Fetch(batchSize int, prefetch bool, opts ...ConsumingOpt) ([]*Msg, error) {
	if batchSize > maxBatchSize || batchSize < 1 {
		return nil, memphisError(errors.New("Batch size can not be greater than " + strconv.Itoa(maxBatchSize) + " or less than 1"))
	}

	defaultOpts := getDefaultConsumingOptions()

	for _, opt := range opts {
		if opt != nil {
			if err := opt(&defaultOpts); err != nil {
				return nil, memphisError(err)
			}
		}
	}

	c.BatchSize = batchSize
	var msgs []*Msg
	if len(c.dlsMsgs) > 0 {
		c.dlsMsgsMutex.Lock()
		if len(c.dlsMsgs) <= batchSize {
			msgs = c.dlsMsgs
			c.dlsMsgs = []*Msg{}
		} else {
			msgs = c.dlsMsgs[:batchSize-1]
			c.dlsMsgs = c.dlsMsgs[batchSize-1:]
		}
		c.dlsMsgsMutex.Unlock()
		return msgs, nil
	}

	c.conn.prefetchedMsgs.lock.Lock()
	lowerCaseStationName := getLowerCaseName(c.stationName)
	if prefetchedMsgsForStation, ok := c.conn.prefetchedMsgs.msgs[lowerCaseStationName]; ok {
		if prefetchedMsgsForCG, ok := prefetchedMsgsForStation[c.ConsumerGroup]; ok {
			if len(prefetchedMsgsForCG) > 0 {
				if len(prefetchedMsgsForCG) <= batchSize {
					msgs = prefetchedMsgsForCG
					prefetchedMsgsForCG = []*Msg{}
				} else {
					msgs = prefetchedMsgsForCG[:batchSize-1]
					prefetchedMsgsForCG = prefetchedMsgsForCG[batchSize-1:]
				}
				c.conn.prefetchedMsgs.msgs[lowerCaseStationName][c.ConsumerGroup] = prefetchedMsgsForCG
			}
		}
	}
	c.conn.prefetchedMsgs.lock.Unlock()
	if prefetch {
		go c.prefetchMsgs(defaultOpts.ConsumerPartitionKey, defaultOpts.ConsumerPartitionNumber)
	}
	if len(msgs) > 0 {
		return msgs, nil
	}
	return c.fetchSubscriprionWithTimeout(defaultOpts.ConsumerPartitionKey, defaultOpts.ConsumerPartitionNumber)
}

func (c *Consumer) prefetchMsgs(partitionKey string, partitionNumber int) {
	c.conn.prefetchedMsgs.lock.Lock()
	defer c.conn.prefetchedMsgs.lock.Unlock()
	lowerCaseStationName := getLowerCaseName(c.stationName)
	if _, ok := c.conn.prefetchedMsgs.msgs[lowerCaseStationName]; !ok {
		c.conn.prefetchedMsgs.msgs[lowerCaseStationName] = make(map[string][]*Msg)
	}
	if _, ok := c.conn.prefetchedMsgs.msgs[lowerCaseStationName][c.ConsumerGroup]; !ok {
		c.conn.prefetchedMsgs.msgs[lowerCaseStationName][c.ConsumerGroup] = make([]*Msg, 0)
	}
	msgs, err := c.fetchSubscriprionWithTimeout(partitionKey, partitionNumber)
	if err == nil {
		c.conn.prefetchedMsgs.msgs[lowerCaseStationName][c.ConsumerGroup] = append(c.conn.prefetchedMsgs.msgs[lowerCaseStationName][c.ConsumerGroup], msgs...)
	}
}

func (c *Consumer) dlsSubscriptionInit() error {
	var err error
	_, err = c.conn.brokerQueueSubscribe(c.getDlsSubjName(), c.getDlsQueueName(), c.createDlsMsgHandler())
	return memphisError(err)
}

func (c *Consumer) createDlsMsgHandler() nats.MsgHandler {
	return func(msg *nats.Msg) {
		// if a consume function is active
		if c.dlsHandlerFunc != nil {
			dlsMsg := []*Msg{{msg: msg, conn: c.conn, cgName: c.ConsumerGroup}}
			c.dlsHandlerFunc(dlsMsg, nil, nil)
		} else {
			// for fetch function
			internalStationName := getInternalName(c.stationName)
			c.dlsMsgsMutex.Lock()
			if len(c.dlsMsgs) > 9999 {
				indexToInsert := c.dlsCurrentIndex
				if indexToInsert >= 10000 {
					indexToInsert = indexToInsert % 10000
				}
				c.dlsMsgs[indexToInsert] = &Msg{msg: msg, conn: c.conn, cgName: c.ConsumerGroup, internalStationName: internalStationName}
			} else {
				c.dlsMsgs = append(c.dlsMsgs, &Msg{msg: msg, conn: c.conn, cgName: c.ConsumerGroup, internalStationName: internalStationName})
			}
			c.dlsCurrentIndex = c.dlsCurrentIndex + 1
			c.dlsMsgsMutex.Unlock()
		}
	}
}

func (c *Consumer) getDlsSubjName() string {
	stationName := getInternalName(c.stationName)
	consumerGroup := getInternalName(c.ConsumerGroup)
	return fmt.Sprintf("%v_%v.%v", dlsSubjPrefix, stationName, consumerGroup)
}

func (c *Consumer) getDlsQueueName() string {
	return c.getDlsSubjName()
}

// Destroy - destroy this consumer.
func (c *Consumer) Destroy(options ...RequestOpt) error {
	if err := c.conn.removeSchemaUpdatesListener(c.stationName); err != nil {
		return memphisError(err)
	}
	if c.consumeActive {
		c.StopConsume()
	}
	if c.subscriptionActive {
		c.pingQuit <- struct{}{}
	}

	c.conn.unCacheConsumer(c)
	return c.conn.destroy(c, options...)
}

func (c *Consumer) getCreationSubject() string {
	return "$memphis_consumer_creations"
}

func (c *Consumer) getCreationReq() any {
	return createConsumerReq{
		Name:                     c.Name,
		StationName:              c.stationName,
		ConnectionId:             c.conn.ConnId,
		ConsumerType:             "application",
		ConsumerGroup:            c.ConsumerGroup,
		MaxAckTimeMillis:         int(c.MaxAckTime.Milliseconds()),
		MaxMsgDeliveries:         c.MaxMsgDeliveries,
		Username:                 c.conn.username,
		StartConsumeFromSequence: c.StartConsumeFromSequence,
		LastMessages:             c.LastMessages,
		RequestVersion:           lastConsumerCreationReqVersion,
		AppId:                    applicationId,
		SdkLang:                  "go",
	}
}

func (c *Consumer) handleCreationResp(resp []byte) error {
	cr := &createConsumerResp{}
	sn := getInternalName(c.stationName)
	err := json.Unmarshal(resp, cr)
	if err != nil {
		// unmarshal failed, we may be dealing with an old broker
		c.conn.stationPartitions[sn] = &PartitionsUpdate{}
		return defaultHandleCreationResp(resp)
	}

	if cr.Err != "" {
		return memphisError(errors.New(cr.Err))
	}

	c.conn.stationUpdatesMu.Lock()
	sd := &c.conn.stationUpdatesSubs[sn].schemaDetails
	sd.handleSchemaUpdateInit(cr.SchemaUpdateInit)
	c.conn.stationUpdatesMu.Unlock()

	c.conn.stationPartitions[sn] = &cr.PartitionsUpdate
	if len(cr.PartitionsUpdate.PartitionsList) > 0 {
		c.PartitionGenerator = newRoundRobinGenerator(cr.PartitionsUpdate.PartitionsList)
	}

	return nil
}

func (c *Consumer) getDestructionSubject() string {
	return "$memphis_consumer_destructions"
}

func (c *Consumer) getDestructionReq() any {
	return removeConsumerReq{Name: c.Name, StationName: c.stationName, Username: c.conn.username, ConnectionId: c.conn.ConnId, RequestVersion: lastConsumerDestroyReqVersion}
}

// ConsumerGroup - consumer group name, default is "".
func ConsumerGroup(cg string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.ConsumerGroup = cg
		return nil
	}
}

// PullInterval - interval between pulls, default is 1 second.
func PullInterval(pullInterval time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.PullInterval = pullInterval
		return nil
	}
}

// BatchSize - pull batch size.
func BatchSize(batchSize int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.BatchSize = batchSize
		return nil
	}
}

// BatchMaxWaitTime - max time to wait between pulls, defauls is 5 seconds.
func BatchMaxWaitTime(batchMaxWaitTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		if batchMaxWaitTime < 1*time.Millisecond {
			batchMaxWaitTime = 1 * time.Millisecond
		}
		opts.BatchMaxTimeToWait = batchMaxWaitTime
		return nil
	}
}

// MaxAckTime - max time for ack a message, in case a message not acked within this time period memphis will resend it.
func MaxAckTime(maxAckTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxAckTime = maxAckTime
		return nil
	}
}

// MaxMsgDeliveries - max number of message deliveries, by default is 2.
func MaxMsgDeliveries(maxMsgDeliveries int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxMsgDeliveries = maxMsgDeliveries
		return nil
	}
}

// Deprecated: will be stopped to be supported after November 1'st, 2023.
// ConsumerGenUniqueSuffix - whether to generate a unique suffix for this consumer.
func ConsumerGenUniqueSuffix() ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		log.Printf("Deprecation warning: ConsumerGenUniqueSuffix will be stopped to be supported after November 1'st, 2023.")
		opts.GenUniqueSuffix = true
		return nil
	}
}

// ConsumerErrorHandler - handler for consumer errors.
func ConsumerErrorHandler(ceh ConsumerErrHandler) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.ErrHandler = ceh
		return nil
	}
}

func StartConsumeFromSequence(startConsumeFromSequence uint64) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.StartConsumeFromSequence = startConsumeFromSequence
		return nil
	}
}

func LastMessages(lastMessages int64) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.LastMessages = lastMessages
		return nil
	}
}

// ConsumerTimeoutRetry - number of retries for consumer timeout. the default value is 5
func ConsumerTimeoutRetry(timeoutRetry int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.TimeoutRetry = timeoutRetry
		return nil
	}
}

func (con *Conn) cacheConsumer(c *Consumer) {
	cm := con.getConsumersMap()
	cm.setConsumer(c)
}

func (con *Conn) unCacheConsumer(c *Consumer) {
	cn := fmt.Sprintf("%s_%s", c.stationName, c.realName)
	cm := con.getConsumersMap()
	if cm.getConsumer(cn) == nil {
		cm.unsetConsumer(cn)
	}
}
