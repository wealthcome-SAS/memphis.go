package memphis

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
)

const (
	consumerDefaultPingInterval = 30 * time.Second
)

type Consumer struct {
	Name               string
	ConsumerGroup      string
	PullInterval       time.Duration
	BatchSize          int
	BatchMaxTimeToWait time.Duration
	MaxAckTime         time.Duration
	MaxMsgDeliveries   int
	conn               *Conn
	stationName        string
	subscription       *nats.Subscription
	pingInterval       time.Duration
	subscriptionActive bool
	firstFetch         bool
	consumeActive      bool
	consumeQuit        chan struct{}
	pingQuit           chan struct{}
}

type Msg struct {
	msg *nats.Msg
}

func (m *Msg) Data() []byte {
	return m.msg.Data
}

func (m *Msg) Ack() error {
	return m.msg.Ack()
}

type createConsumerReq struct {
	Name             string `json:"name"`
	StationName      string `json:"station_name"`
	ConnectionId     string `json:"connection_id"`
	ConsumerType     string `json:"consumer_type"`
	ConsumerGroup    string `json:"consumers_group"`
	MaxAckTimeMillis int    `json:"max_ack_time_ms"`
	MaxMsgDeliveries int    `json:"max_msg_deliveries"`
}

type removeConsumerReq struct {
	Name        string `json:"name"`
	StationName string `json:"station_name"`
}

type ConsumerOpts struct {
	Name               string
	StationName        string
	ConsumerGroup      string
	PullInterval       time.Duration
	BatchSize          int
	BatchMaxTimeToWait time.Duration
	MaxAckTime         time.Duration
	MaxMsgDeliveries   int
}

func GetDefaultConsumerOptions() ConsumerOpts {
	return ConsumerOpts{
		PullInterval:       1 * time.Second,
		BatchSize:          10,
		BatchMaxTimeToWait: 5 * time.Second,
		MaxAckTime:         30 * time.Second,
		MaxMsgDeliveries:   10,
	}
}

type ConsumerOpt func(*ConsumerOpts) error

func (c *Conn) CreateConsumer(stationName, consumerName string, opts ...ConsumerOpt) (*Consumer, error) {
	defaultOpts := GetDefaultConsumerOptions()

	defaultOpts.Name = consumerName
	defaultOpts.StationName = stationName
	defaultOpts.ConsumerGroup = consumerName

	for _, opt := range opts {
		if opt != nil {
			if err := opt(&defaultOpts); err != nil {
				return nil, err
			}
		}
	}

	return defaultOpts.CreateConsumer(c)
}

func (opts *ConsumerOpts) CreateConsumer(c *Conn) (*Consumer, error) {
	consumer := Consumer{Name: opts.Name,
		ConsumerGroup:      opts.ConsumerGroup,
		PullInterval:       opts.PullInterval,
		BatchSize:          opts.BatchSize,
		MaxAckTime:         opts.MaxAckTime,
		MaxMsgDeliveries:   opts.MaxMsgDeliveries,
		BatchMaxTimeToWait: opts.BatchMaxTimeToWait,
		conn:               c,
		stationName:        opts.StationName}

	err := c.create(&consumer)
	if err != nil {
		return nil, err
	}

	consumer.firstFetch = true
	consumer.consumeQuit = make(chan struct{}, 1)
	consumer.pingQuit = make(chan struct{}, 1)

	consumer.pingInterval = consumerDefaultPingInterval

	subj := consumer.stationName + ".final"

	consumer.subscription, err = c.brokerSubscribe(subj,
		consumer.ConsumerGroup,
		nats.ManualAck(),
		nats.AckWait(consumer.MaxAckTime),
		nats.MaxRequestExpires(consumer.BatchMaxTimeToWait),
		nats.MaxRequestBatch(opts.BatchSize))
	if err != nil {
		return nil, err
	}

	consumer.subscriptionActive = true

	go consumer.pingConsumer()

	return &consumer, err
}

func (s *Station) CreateConsumer(name string, opts ...ConsumerOpt) (*Consumer, error) {
	return s.conn.CreateConsumer(s.Name, name, opts...)
}

func (c *Consumer) pingConsumer() {
	ticker := time.NewTicker(c.pingInterval)
	if !c.subscriptionActive {
		log.Fatal("started ping for inactive subscription")
	}

	for {
		select {
		case <-ticker.C:
			_, err := c.subscription.ConsumerInfo()
			if err != nil {
				c.subscriptionActive = false
				log.Error("Station unreachable")
				return
			}
		case <-c.pingQuit:
			ticker.Stop()
			return
		}
	}

}

type ConsumeHandler func([]*Msg, error)

func (c *Consumer) Consume(handlerFunc ConsumeHandler) {
	ticker := time.NewTicker(c.PullInterval)

	if c.firstFetch {
		c.firstFetch = false
		msgs, err := c.fetchSubscription()
		go handlerFunc(msgs, err)
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				msgs, err := c.fetchSubscription()
				go handlerFunc(msgs, err)
			case <-c.consumeQuit:
				ticker.Stop()
				return
			}
		}
	}()
	c.consumeActive = true
}

func (c *Consumer) StopConsume() {
	if !c.consumeActive {
		log.Error("Consume is inactive")
		return
	}
	c.consumeQuit <- struct{}{}
	c.consumeActive = false
}

func (c *Consumer) fetchSubscription() ([]*Msg, error) {
	if !c.subscriptionActive {
		return nil, errors.New("Station unreachable")
	}

	subscription := c.subscription
	batchSize := c.BatchSize
	msgs, err := subscription.Fetch(batchSize)
	if err != nil {
		return nil, err
	}

	wrappedMsgs := make([]*Msg, 0, batchSize)

	for _, msg := range msgs {
		wrappedMsgs = append(wrappedMsgs, &Msg{msg: msg})
	}
	return wrappedMsgs, nil
}

type FetchResult struct {
	msgs []*Msg
	err  error
}

func (c *Consumer) fetchSubscriprionWithTimeout() ([]*Msg, error) {
	timeoutDuration := c.BatchMaxTimeToWait
	out := make(chan FetchResult, 1)
	go func() {
		msgs, err := c.fetchSubscription()
		out <- FetchResult{msgs: msgs, err: err}
	}()
	select {
	case <-time.After(timeoutDuration):
		return nil, errors.New("Fetch timed out")
	case fetchRes := <-out:
		return fetchRes.msgs, fetchRes.err

	}
}

func (c *Consumer) Fetch() ([]*Msg, error) {
	if c.firstFetch {
		c.firstFetch = false
	}

	return c.fetchSubscriprionWithTimeout()
}

func (c *Consumer) Destroy() error {
	if c.consumeActive {
		c.StopConsume()
	}
	if c.subscriptionActive {
		c.pingQuit <- struct{}{}
	}

	return c.conn.destroy(c)
}

func (c *Consumer) getCreationApiPath() string {
	return "/api/consumers/createConsumer"
}

func (c *Consumer) getCreationReq() any {
	return createConsumerReq{
		Name:             c.Name,
		StationName:      c.stationName,
		ConnectionId:     c.conn.ConnId,
		ConsumerType:     "application",
		ConsumerGroup:    c.ConsumerGroup,
		MaxAckTimeMillis: int(c.MaxAckTime.Milliseconds()),
		MaxMsgDeliveries: c.MaxMsgDeliveries,
	}
}

func (p *Consumer) getDestructionApiPath() string {
	return "/api/consumers/destroyConsumer"
}

func (p *Consumer) getDestructionReq() any {
	return removeConsumerReq{Name: p.Name, StationName: p.stationName}
}

func ConsumerName(name string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.Name = name
		return nil
	}
}

func StationNameOpt(stationName string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.StationName = stationName
		return nil
	}
}
func ConsumerGroup(cg string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.ConsumerGroup = cg
		return nil
	}
}
func PullInterval(pullInterval time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.PullInterval = pullInterval
		return nil
	}
}
func BatchSize(batchSize int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.BatchSize = batchSize
		return nil
	}
}
func BatchMaxWaitTime(batchMaxWaitTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.BatchMaxTimeToWait = batchMaxWaitTime
		return nil
	}
}
func MaxAckTime(maxAckTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxAckTime = maxAckTime
		return nil
	}
}
func MaxMsgDeliveries(maxMsgDeliveries int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxMsgDeliveries = maxMsgDeliveries
		return nil
	}
}