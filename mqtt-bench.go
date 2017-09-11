package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

const BASE_TOPIC string = "/mqtt-bench/benchmark"

var Debug bool = false

// Allows to hold the processing result of DefaultHandler at Subscribe for Apollo
var DefaultHandlerResults []*SubscribeResult

// Execution options
type ExecOptions struct {
	Broker            string     // Broker URI
	Qos               byte       // QoS(0|1|2)
	Retain            bool       // Retain
	Topic             string     // Topic
	Username          string     // User ID
	Password          string     // Password
	CertConfig        CertConfig // Authentication definition
	ClientNum         int        // Number of concurrent clients
	Count             int        // Number of messages per client
	MessageSize       int        // 1 Message size (byte)
	UseDefaultHandler bool       // Whether to use the default MessageHandler instead of Subscriber individually
	PreTime           int        // Wait time before execution (ms)
	IntervalTime      int        // Execution interval time for each message (ms)
	CleanSession      bool       // Use MQTT clean session or not
}

// Authentication settings
type CertConfig interface{}

// Server authentication setting
type ServerCertConfig struct {
	CertConfig
	ServerCertFile string // Server certificate file
}

// Client authentication settings
type ClientCertConfig struct {
	CertConfig
	RootCAFile     string // Root certificate file
	ClientCertFile string // Client certificate file
	ClientKeyFile  string // Client public key file
}

// Generate TLS settings for server certificate.
//   serverCertFile : Server certificate file
func CreateServerTlsConfig(serverCertFile string) *tls.Config {
	certpool := x509.NewCertPool()
	pem, err := ioutil.ReadFile(serverCertFile)
	if err == nil {
		certpool.AppendCertsFromPEM(pem)
	}

	return &tls.Config{
		RootCAs: certpool,
	}
}

// Generate TLS configuration for client certificate.。
//   rootCAFile     : Root certificate file
//   clientCertFile : Client certificate file
//   clientKeyFile  : Client public key file
func CreateClientTlsConfig(rootCAFile string, clientCertFile string, clientKeyFile string) *tls.Config {
	certpool := x509.NewCertPool()
	rootCA, err := ioutil.ReadFile(rootCAFile)
	if err == nil {
		certpool.AppendCertsFromPEM(rootCA)
	}

	cert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
	if err != nil {
		panic(err)
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		panic(err)
	}

	return &tls.Config{
		RootCAs:            certpool,
		ClientAuth:         tls.NoClientCert,
		ClientCAs:          nil,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
	}
}

// Execute
func Execute(exec func(clients []MQTT.Client, opts ExecOptions, param ...string) int, opts ExecOptions) {
	message := CreateFixedSizeMessage(opts.MessageSize)

	// Initialize slice
	DefaultHandlerResults = make([]*SubscribeResult, opts.ClientNum)

	clients := make([]MQTT.Client, opts.ClientNum)
	hasErr := false
	for i := 0; i < opts.ClientNum; i++ {
		client := Connect(i, opts)
		if client == nil {
			hasErr = true
			break
		}
		clients[i] = client
	}

	// If there is a connection error, perform disconnect of the connected client, and end the processing
	if hasErr {
		for i := 0; i < len(clients); i++ {
			client := clients[i]
			if client != nil {
				client.Disconnect(10)
			}
		}
		return
	}

	// Wait for a certain period of time to stabilize
	time.Sleep(time.Duration(opts.PreTime) * time.Millisecond)

	fmt.Printf("%s Start benchmark\n", time.Now())

	startTime := time.Now()
	totalCount := exec(clients, opts, message)
	endTime := time.Now()

	fmt.Printf("%s End benchmark\n", time.Now())

	// Since it takes time to cut, asynchronously process
	DisconnectClients(clients)

	// And prints the result
	duration := (endTime.Sub(startTime)).Nanoseconds() / int64(1000000) // nanosecond -> millisecond
	throughput := float64(totalCount) / float64(duration) * 1000        // messages/sec
	fmt.Printf("\nResult : broker=%s, clients=%d, totalCount=%d, duration=%dms, throughput=%.2fmessages/sec\n",
		opts.Broker, opts.ClientNum, totalCount, duration, throughput)
}

// Process publish for all clients
// Returns the number of sent messages (in principle, it will be the number of clients)
func PublishAllClient(clients []MQTT.Client, opts ExecOptions, param ...string) int {
	message := param[0]

	wg := new(sync.WaitGroup)

	totalCount := 0
	for id := 0; id < len(clients); id++ {
		wg.Add(1)

		client := clients[id]

		go func(clientId int) {
			defer wg.Done()

			for index := 0; index < opts.Count; index++ {
				topic := fmt.Sprintf(opts.Topic+"/%d", clientId)

				if Debug {
					fmt.Printf("Publish : id=%d, count=%d, topic=%s\n", clientId, index, topic)
				}
				Publish(client, topic, opts.Qos, opts.Retain, message)
				totalCount++

				if opts.IntervalTime > 0 {
					time.Sleep(time.Duration(opts.IntervalTime) * time.Millisecond)
				}
			}
		}(id)
	}

	wg.Wait()

	return totalCount
}

// Send a message
func Publish(client MQTT.Client, topic string, qos byte, retain bool, message string) {
	token := client.Publish(topic, qos, retain, message)

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Publish error: %s\n", token.Error())
	}
}

// Process subscribe to all clients
// Wait to receive a message for the specified number of counts (it will not be counted if a message can not be acquired)
// In this process, Subscribe process is performed while continuing Publish
func SubscribeAllClient(clients []MQTT.Client, opts ExecOptions, param ...string) int {
	wg := new(sync.WaitGroup)

	results := make([]*SubscribeResult, len(clients))
	for id := 0; id < len(clients); id++ {
		wg.Add(1)

		client := clients[id]
		topic := fmt.Sprintf(opts.Topic+"/%d", id)

		results[id] = Subscribe(client, topic, opts.Qos)

		// DefaultHandlerを利用する場合は、Subscribe個別のHandlerではなく、
		// DefaultHandlerの処理結果を参照する。
		if opts.UseDefaultHandler == true {
			results[id] = DefaultHandlerResults[id]
		}

		go func(clientId int) {
			defer wg.Done()

			var loop int = 0
			for results[clientId].Count < opts.Count {
				loop++

				if Debug {
					fmt.Printf("Subscribe : id=%d, count=%d, topic=%s\n", clientId, results[clientId].Count, topic)
				}

				if opts.IntervalTime > 0 {
					time.Sleep(time.Duration(opts.IntervalTime) * time.Millisecond)
				} else {
					// Wait for at least 1000 nanoseconds (0.001 milliseconds) to lower the load due to the for statement
					time.Sleep(1000 * time.Nanosecond)
				}

				// To avoid an infinite loop, if it reaches 100 times the specified Count, it ends with an error
				if loop >= opts.Count*100 {
					panic("Subscribe error : Not finished in the max count. It may not be received the message.")
				}
			}
		}(id)
	}

	wg.Wait()

	// Count the number of received messages
	totalCount := 0
	for id := 0; id < len(results); id++ {
		totalCount += results[id].Count
	}

	return totalCount
}

// Subscribe processing result
type SubscribeResult struct {
	Count int // Number of messages received
}

// Receive a message
func Subscribe(client MQTT.Client, topic string, qos byte) *SubscribeResult {
	var result *SubscribeResult = &SubscribeResult{}
	result.Count = 0

	var handler MQTT.MessageHandler = func(client MQTT.Client, msg MQTT.Message) {
		result.Count++
		if Debug {
			fmt.Printf("Received message : topic=%s, message=%s\n", msg.Topic(), msg.Payload())
		}
	}

	token := client.Subscribe(topic, qos, handler)

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Subscribe error: %s\n", token.Error())
	}

	return result
}

// Generate a fixed size message.
func CreateFixedSizeMessage(size int) string {
	var buffer bytes.Buffer
	for i := 0; i < size; i++ {
		buffer.WriteString(strconv.Itoa(i % 10))
	}

	message := buffer.String()
	return message
}

// Connects to the specified Broker and returns its MQTT client
// If connection fails, return nil
func Connect(id int, execOpts ExecOptions) MQTT.Client {

	// When multiple ClientID's are duplicated in a plurality of processes,
	// since it becomes a problem on the Broker side, IDs are allocated by using the process ID.
	// mqttbench<hexadecimal_value_of_process_ID>-<serial_number_of_client>
	pid := strconv.FormatInt(int64(os.Getpid()), 16)
	clientId := fmt.Sprintf("mqttbench%s-%d", pid, id)

	opts := MQTT.NewClientOptions()
	opts.AddBroker(execOpts.Broker)
	opts.SetClientID(clientId)
	opts.SetCleanSession(execOpts.CleanSession)

	if execOpts.Username != "" {
		opts.SetUsername(execOpts.Username)
	}
	if execOpts.Password != "" {
		opts.SetPassword(execOpts.Password)
	}

	// TLS setting
	certConfig := execOpts.CertConfig
	switch c := certConfig.(type) {
	case ServerCertConfig:
		tlsConfig := CreateServerTlsConfig(c.ServerCertFile)
		opts.SetTLSConfig(tlsConfig)
	case ClientCertConfig:
		tlsConfig := CreateClientTlsConfig(c.RootCAFile, c.ClientCertFile, c.ClientKeyFile)
		opts.SetTLSConfig(tlsConfig)
	default:
		// do nothing.
	}

	if execOpts.UseDefaultHandler == true {
		// In the case of Apollo (using 1.7.1), you can not subscribe unless you specify DefaultPublishHandler
		// However, it is noted that retained messages are retained only once for the first time even if they are specified,
		// and empty for the second and subsequent accesses
		var result *SubscribeResult = &SubscribeResult{}
		result.Count = 0

		var handler MQTT.MessageHandler = func(client MQTT.Client, msg MQTT.Message) {
			result.Count++
			if Debug {
				fmt.Printf("Received at defaultHandler : topic=%s, message=%s\n", msg.Topic(), msg.Payload())
			}
		}
		opts.SetDefaultPublishHandler(handler)

		DefaultHandlerResults[id] = result
	}

	client := MQTT.NewClient(opts)
	token := client.Connect()

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Connected error: %s\n", token.Error())
		return nil
	}

	return client
}

// Disconnect all clients from the Broker
func DisconnectClients(clients []MQTT.Client) {
	for _, client := range clients {
		client.Disconnect(10)
	}
}

// Check the existence of the file.
// Returns true if the file exists, false if it does not exist.
//   filePath : Path of file to check for existence
func FileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

func main() {
	broker := flag.String("broker", "tcp://{host}:{port}", "URI of MQTT broker (required)")
	action := flag.String("action", "p|pub or s|sub", "Publish or Subscribe or Subscribe(with publishing) (required)")
	qos := flag.Int("qos", 0, "MQTT QoS(0|1|2)")
	retain := flag.Bool("retain", false, "MQTT Retain")
	topic := flag.String("topic", BASE_TOPIC, "Base topic")
	username := flag.String("broker-username", "", "Username for connecting to the MQTT broker")
	password := flag.String("broker-password", "", "Password for connecting to the MQTT broker")
	tlsMode := flag.String("tls", "", "TLS mode. 'server:certFile' or 'client:rootCAFile,clientCertFile,clientKeyFile'")
	clients := flag.Int("clients", 10, "Number of clients")
	count := flag.Int("count", 100, "Number of loops per client")
	size := flag.Int("size", 1024, "Message size per publish (byte)")
	useDefaultHandler := flag.Bool("support-unknown-received", false, "Using default messageHandler for a message that does not match any known subscriptions")
	preTime := flag.Int("pretime", 3000, "Pre wait time (ms)")
	intervalTime := flag.Int("intervaltime", 0, "Interval time per message (ms)")
	debug := flag.Bool("x", false, "Debug mode")
	cleanSess := flag.Bool("clean-session", false, "MQTT clean session mode")

	flag.Parse()

	if len(os.Args) <= 1 {
		flag.Usage()
		return
	}

	// validate "broker"
	if broker == nil || *broker == "" || *broker == "tcp://{host}:{port}" {
		fmt.Printf("Invalid argument : -broker -> %s\n", *broker)
		return
	}

	// validate "action"
	var method string = ""
	if *action == "p" || *action == "pub" {
		method = "pub"
	} else if *action == "s" || *action == "sub" {
		method = "sub"
	}

	if method != "pub" && method != "sub" {
		fmt.Printf("Invalid argument : -action -> %s\n", *action)
		return
	}

	// parse TLS mode
	var certConfig CertConfig = nil
	if *tlsMode == "" {
		// nil
	} else if strings.HasPrefix(*tlsMode, "server:") {
		var strArray = strings.Split(*tlsMode, "server:")
		serverCertFile := strings.TrimSpace(strArray[1])
		if FileExists(serverCertFile) == false {
			fmt.Printf("File is not found. : certFile -> %s\n", serverCertFile)
			return
		}

		certConfig = ServerCertConfig{
			ServerCertFile: serverCertFile}
	} else if strings.HasPrefix(*tlsMode, "client:") {
		var strArray = strings.Split(*tlsMode, "client:")
		var configArray = strings.Split(strArray[1], ",")
		rootCAFile := strings.TrimSpace(configArray[0])
		clientCertFile := strings.TrimSpace(configArray[1])
		clientKeyFile := strings.TrimSpace(configArray[2])
		if FileExists(rootCAFile) == false {
			fmt.Printf("File is not found. : rootCAFile -> %s\n", rootCAFile)
			return
		}
		if FileExists(clientCertFile) == false {
			fmt.Printf("File is not found. : clientCertFile -> %s\n", clientCertFile)
			return
		}
		if FileExists(clientKeyFile) == false {
			fmt.Printf("File is not found. : clientKeyFile -> %s\n", clientKeyFile)
			return
		}

		certConfig = ClientCertConfig{
			RootCAFile:     rootCAFile,
			ClientCertFile: clientCertFile,
			ClientKeyFile:  clientKeyFile}
	} else {
		// nil
	}

	execOpts := ExecOptions{}
	execOpts.Broker = *broker
	execOpts.Qos = byte(*qos)
	execOpts.Retain = *retain
	execOpts.Topic = *topic
	execOpts.Username = *username
	execOpts.Password = *password
	execOpts.CertConfig = certConfig
	execOpts.ClientNum = *clients
	execOpts.Count = *count
	execOpts.MessageSize = *size
	execOpts.UseDefaultHandler = *useDefaultHandler
	execOpts.PreTime = *preTime
	execOpts.IntervalTime = *intervalTime
	execOpts.CleanSession = *cleanSess

	Debug = *debug

	MQTT.ERROR = log.New(os.Stderr, "MQTT_ERR:", log.LstdFlags)

	switch method {
	case "pub":
		Execute(PublishAllClient, execOpts)
	case "sub":
		Execute(SubscribeAllClient, execOpts)
	}
}
