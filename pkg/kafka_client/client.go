package kafka_client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
	"log"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

const maxEarliest int64 = 100
const network = "tcp"
const debugLogLevel = "debug"
const errorLogLevel = "error"
const dialerTimeout = 10 * time.Second

type Options struct {
	BootstrapServers   string `json:"bootstrapServers"`
	SecurityProtocol   string `json:"securityProtocol"`
	SaslMechanisms     string `json:"saslMechanisms"`
	SaslUsername       string `json:"saslUsername"`
	SaslPassword       string `json:"saslPassword"`
	HealthcheckTimeout int32  `json:"healthcheckTimeout"`
	LogLevel           string `json:"logLevel"`
}

type KafkaClient struct {
	Dialer             *kafka.Dialer
	Reader             *kafka.Reader
	BootstrapServers   string
	TimestampMode      string
	SecurityProtocol   string
	SaslMechanisms     string
	SaslUsername       string
	SaslPassword       string
	LogLevel           string
	HealthcheckTimeout int32
}

type KafkaMessage struct {
	Value     map[string]float64
	Timestamp time.Time
	Offset    int64
}

func NewKafkaClient(options Options) KafkaClient {
	client := KafkaClient{
		BootstrapServers:   options.BootstrapServers,
		SecurityProtocol:   options.SecurityProtocol,
		SaslMechanisms:     options.SaslMechanisms,
		SaslUsername:       options.SaslUsername,
		SaslPassword:       options.SaslPassword,
		LogLevel:           options.LogLevel,
		HealthcheckTimeout: options.HealthcheckTimeout,
	}
	return client
}

func (client *KafkaClient) NewConnection() error {
	var err error
	var mechanism sasl.Mechanism

	if client.SaslMechanisms != "" {
		mechanism, err = getSASLMechanism(client)
		if err != nil {
			return fmt.Errorf("unable to get sasl mechanism: %w", err)
		}
	}

	dialer := &kafka.Dialer{
		Timeout: dialerTimeout,
	}

	if mechanism != nil {
		dialer.SASLMechanism = mechanism
	}

	if client.SecurityProtocol == "SASL_SSL" {
		dialer.TLS = &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
	}

	client.Dialer = dialer

	return nil
}

func (client *KafkaClient) newReader(topic string, partition int) *kafka.Reader {
	logger, errorLogger := getKafkaLogger(client.LogLevel)

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        strings.Split(client.BootstrapServers, ","),
		Topic:          topic,
		Partition:      partition,
		Dialer:         client.Dialer,
		CommitInterval: 0,
		Logger:         logger,
		ErrorLogger:    errorLogger,
	})

	return reader
}

func (client *KafkaClient) TopicAssign(
	topic string,
	partition int32,
	autoOffsetReset string,
	timestampMode string,
) error {
	client.TimestampMode = timestampMode

	var offset int64
	var high, low int64

	switch autoOffsetReset {
	case "latest":
		offset = kafka.LastOffset
	case "earliest":
		// We have to connect to the partition leader to read offsets
		conn, err := client.Dialer.DialLeader(context.Background(), network, client.BootstrapServers, topic, int(partition))
		if err != nil {
			return fmt.Errorf("unable to dial leader: %w", err)
		}
		defer conn.Close()

		low, high, err = conn.ReadOffsets()
		if err != nil {
			return fmt.Errorf("unable to read offsets: %w", err)
		}

		if high-low > maxEarliest {
			offset = high - maxEarliest
		} else {
			offset = low
		}
	default:
		offset = kafka.LastOffset
	}

	client.Reader = client.newReader(topic, int(partition))
	if err := client.Reader.SetOffset(offset); err != nil {
		return fmt.Errorf("unable to set offset: %w", err)
	}

	return nil
}

func (client *KafkaClient) ConsumerPull(ctx context.Context) (KafkaMessage, error) {
	var message KafkaMessage

	msg, err := client.Reader.ReadMessage(ctx)
	if err != nil {
		return message, fmt.Errorf("error reading message from Kafka: %w", err)
	}

	if err := json.Unmarshal(msg.Value, &message.Value); err != nil {
		return message, fmt.Errorf("error unmarshalling message: %w", err)
	}

	message.Offset = msg.Offset
	message.Timestamp = msg.Time

	return message, nil
}

func (client *KafkaClient) HealthCheck() error {
	if err := client.NewConnection(); err != nil {
		return fmt.Errorf("unable to initialize Kafka client: %w", err)
	}
	var conn *kafka.Conn
	var err error

	// It is better to try several times due to possible network issues
	timeout := time.After(time.Duration(client.HealthcheckTimeout) * time.Millisecond)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("health check timed out after %d ms: %w", client.HealthcheckTimeout, err)
		case <-ticker.C:
			conn, err = client.Dialer.Dial(network, client.BootstrapServers)
			if err == nil {
				defer conn.Close()
				if _, err = conn.ReadPartitions(); err != nil {
					return fmt.Errorf("error reading partitions: %w", err)
				}
				return nil
			}
		}
	}
}

func (client *KafkaClient) Dispose() {
	if client.Reader != nil {
		client.Reader.Close()
	}
}

func getSASLMechanism(client *KafkaClient) (sasl.Mechanism, error) {
	switch client.SaslMechanisms {
	case "PLAIN":
		return plain.Mechanism{
			Username: client.SaslUsername,
			Password: client.SaslPassword,
		}, nil
	case "SCRAM-SHA-256":
		return scram.Mechanism(scram.SHA256, client.SaslUsername, client.SaslPassword)
	case "SCRAM-SHA-512":
		return scram.Mechanism(scram.SHA512, client.SaslUsername, client.SaslPassword)
	case "":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported mechanism SASL: %s", client.SaslMechanisms)
	}
}

func (client *KafkaClient) IsTopicExists(ctx context.Context, topicName string) (bool, error) {
	var mechanism sasl.Mechanism
	var err error

	conn := kafka.Client{
		Addr:    kafka.TCP(strings.Split(client.BootstrapServers, ",")...),
		Timeout: dialerTimeout,
	}

	if client.SaslMechanisms != "" {
		mechanism, err = getSASLMechanism(client)
		if err != nil {
			return false, fmt.Errorf("unable to get sasl mechanism: %w", err)
		}
	}

	if mechanism != nil {
		conn.Transport = &kafka.Transport{
			SASL: mechanism,
		}
	}

	meta, err := conn.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return false, fmt.Errorf("unable to get metadata: %w", err)
	}

	topicExists := false
	for _, topic := range meta.Topics {
		if topic.Name == topicName {
			topicExists = true
			break
		}
	}

	return topicExists, nil
}

func getKafkaLogger(level string) (kafka.LoggerFunc, kafka.LoggerFunc) {
	noop := kafka.LoggerFunc(func(msg string, args ...interface{}) {})

	var logger = noop
	var errorLogger = noop

	switch strings.ToLower(level) {
	case debugLogLevel:
		logger = func(msg string, args ...interface{}) {
			log.Printf("[KAFKA DEBUG] "+msg, args...)
		}
		errorLogger = func(msg string, args ...interface{}) {
			log.Printf("[KAFKA ERROR] "+msg, args...)
		}
	case errorLogLevel:
		errorLogger = func(msg string, args ...interface{}) {
			log.Printf("[KAFKA ERROR] "+msg, args...)
		}
	}

	return logger, errorLogger
}
