package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	MQTT "git.eclipse.org/gitroot/paho/org.eclipse.paho.mqtt.golang.git"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const baseTopic string = "/mqtt-bench/benchmark"

var debug = false

// Apollo用に、Subscribe時のDefaultHandlerの処理結果を保持できるようにする。
var defaultHandlerResults []*subscribeResult

// 実行オプション
type execOptions struct {
	Broker            string     // Broker URI
	Qos               byte       // QoS(0|1|2)
	Retain            bool       // Retain
	Topic             string     // Topicのルート
	Username          string     // ユーザID
	Password          string     // パスワード
	CertConfig        certConfig // 認証定義
	ClientNum         int        // クライアントの同時実行数
	Count             int        // 1クライアント当たりのメッセージ数
	MessageSize       int        // 1メッセージのサイズ(byte)
	UseDefaultHandler bool       // Subscriber個別ではなく、デフォルトのMessageHandlerを利用するかどうか
	PreTime           int        // 実行前の待機時間(ms)
	IntervalTime      int        // メッセージ毎の実行間隔時間(ms)
}

// 認証設定
type certConfig interface{}

// サーバ認証設定
type serverCertConfig struct {
	certConfig
	ServerCertFile string // サーバ証明書ファイル
}

// クライアント認証設定
type clientCertConfig struct {
	certConfig
	RootCAFile     string // ルート証明書ファイル
	ClientCertFile string // クライアント証明書ファイル
	ClientKeyFile  string // クライアント公開鍵ファイル
}

// サーバ証明書用のTLS設定を生成する。
//   serverCertFile : サーバ証明書のファイル
func createServerTlsConfig(serverCertFile string) *tls.Config {
	certpool := x509.NewCertPool()
	pem, err := ioutil.ReadFile(serverCertFile)
	if err == nil {
		certpool.AppendCertsFromPEM(pem)
	}

	return &tls.Config{
		RootCAs: certpool,
	}
}

// クライアント証明書用のTLS設定を生成する。
//   rootCAFile     : ルート証明書ファイル
//   clientCertFile : クライアント証明書ファイル
//   clientKeyFile  : クライアント公開鍵ファイル
func createClientTlsConfig(rootCAFile string, clientCertFile string, clientKeyFile string) *tls.Config {
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

// 実行する。
func execute(exec func(clients []*MQTT.Client, opts execOptions, param ...string) int, opts execOptions) {
	message := createFixedSizeMessage(opts.MessageSize)

	// 配列を初期化
	defaultHandlerResults = make([]*subscribeResult, opts.ClientNum)

	clients := make([]*MQTT.Client, opts.ClientNum)
	hasErr := false
	for i := 0; i < opts.ClientNum; i++ {
		client := connect(i, opts)
		if client == nil {
			hasErr = true
			break
		}
		clients[i] = client
	}

	// 接続エラーがあれば、接続済みのクライアントの切断処理を行い、処理を終了する。
	if hasErr {
		for i := 0; i < len(clients); i++ {
			client := clients[i]
			if client != nil {
				disconnect(client)
			}
		}
		return
	}

	// 安定させるために、一定時間待機する。
	time.Sleep(time.Duration(opts.PreTime) * time.Millisecond)

	fmt.Printf("%s Start benchmark\n", time.Now())

	startTime := time.Now()
	totalCount := exec(clients, opts, message)
	endTime := time.Now()

	fmt.Printf("%s End benchmark\n", time.Now())

	// 切断に時間がかかるため、非同期で処理を行う。
	asyncDisconnect(clients)

	// 処理結果を出力する。
	duration := (endTime.Sub(startTime)).Nanoseconds() / int64(1000000) // nanosecond -> millisecond
	throughput := float64(totalCount) / float64(duration) * 1000        // messages/sec
	fmt.Printf("\nResult : broker=%s, clients=%d, totalCount=%d, duration=%dms, throughput=%.2fmessages/sec\n",
		opts.Broker, opts.ClientNum, totalCount, duration, throughput)
}

// 全クライアントに対して、publishの処理を行う。
// 送信したメッセージ数を返す（原則、クライアント数分となる）。
func publishAllClient(clients []*MQTT.Client, opts execOptions, param ...string) int {
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

				if debug {
					fmt.Printf("Publish : id=%d, count=%d, topic=%s\n", clientId, index, topic)
				}
				publish(client, topic, opts.Qos, opts.Retain, message)
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

// メッセージを送信する。
func publish(client *MQTT.Client, topic string, qos byte, retain bool, message string) {
	token := client.Publish(topic, qos, retain, message)

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Publish error: %s\n", token.Error())
	}
}

// 全クライアントに対して、subscribeの処理を行う。
// 指定されたカウント数分、メッセージを受信待ちする（メッセージが取得できない場合はカウントされない）。
// この処理では、Publishし続けながら、Subscribeの処理を行う。
func subscribeAllClient(clients []*MQTT.Client, opts execOptions, param ...string) int {
	wg := new(sync.WaitGroup)

	results := make([]*subscribeResult, len(clients))
	for id := 0; id < len(clients); id++ {
		wg.Add(1)

		client := clients[id]
		topic := fmt.Sprintf(opts.Topic+"/%d", id)

		results[id] = subscribe(client, topic, opts.Qos)

		// DefaultHandlerを利用する場合は、Subscribe個別のHandlerではなく、
		// DefaultHandlerの処理結果を参照する。
		if opts.UseDefaultHandler == true {
			results[id] = defaultHandlerResults[id]
		}

		go func(clientId int) {
			defer wg.Done()

			var loop int = 0
			for results[clientId].Count < opts.Count {
				loop++

				if debug {
					fmt.Printf("Subscribe : id=%d, count=%d, topic=%s\n", clientId, results[clientId].Count, topic)
				}

				if opts.IntervalTime > 0 {
					time.Sleep(time.Duration(opts.IntervalTime) * time.Millisecond)
				} else {
					// for文による負荷を下げるため、最低でも1000ナノ秒（0.001ミリ秒）は待機する。
					time.Sleep(1000 * time.Nanosecond)
				}

				// 無限ループを避けるため、指定されたCountの100倍に達したら、エラーで終了する。
				if loop >= opts.Count*100 {
					panic("Subscribe error : Not finished in the max count. It may not be received the message.")
				}
			}
		}(id)
	}

	wg.Wait()

	// 受信メッセージ数をカウント
	totalCount := 0
	for id := 0; id < len(results); id++ {
		totalCount += results[id].Count
	}

	return totalCount
}

// Subscribeの処理結果
type subscribeResult struct {
	Count int // 受信メッセージ数
}

// メッセージを受信する。
func subscribe(client *MQTT.Client, topic string, qos byte) *subscribeResult {
	var result = &subscribeResult{}
	result.Count = 0

	var handler MQTT.MessageHandler = func(client *MQTT.Client, msg MQTT.Message) {
		result.Count++
		if debug {
			fmt.Printf("Received message : topic=%s, message=%s\n", msg.Topic(), msg.Payload())
		}
	}

	token := client.Subscribe(topic, qos, handler)

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Subscribe error: %s\n", token.Error())
	}

	return result
}

// 固定サイズのメッセージを生成する。
func createFixedSizeMessage(size int) string {
	var buffer bytes.Buffer
	for i := 0; i < size; i++ {
		buffer.WriteString(strconv.Itoa(i % 10))
	}

	message := buffer.String()
	return message
}

// 指定されたBrokerへ接続し、そのMQTTクライアントを返す。
// 接続に失敗した場合は nil を返す。
func connect(id int, execOpts execOptions) *MQTT.Client {

	// 複数プロセスで、ClientIDが重複すると、Broker側で問題となるため、
	// プロセスIDを利用して、IDを割り振る。
	// mqttbench<プロセスIDの16進数値>-<クライアントの連番>
	pid := strconv.FormatInt(int64(os.Getpid()), 16)
	clientId := fmt.Sprintf("mqttbench%s-%d", pid, id)

	opts := MQTT.NewClientOptions()
	opts.AddBroker(execOpts.Broker)
	opts.SetClientID(clientId)

	if execOpts.Username != "" {
		opts.SetUsername(execOpts.Username)
	}
	if execOpts.Password != "" {
		opts.SetPassword(execOpts.Password)
	}

	// TLSの設定
	certConfig := execOpts.CertConfig
	switch c := certConfig.(type) {
	case serverCertConfig:
		tlsConfig := createServerTlsConfig(c.ServerCertFile)
		opts.SetTLSConfig(tlsConfig)
	case clientCertConfig:
		tlsConfig := createClientTlsConfig(c.RootCAFile, c.ClientCertFile, c.ClientKeyFile)
		opts.SetTLSConfig(tlsConfig)
	default:
		// do nothing.
	}

	if execOpts.UseDefaultHandler == true {
		// Apollo(1.7.1利用)の場合、DefaultPublishHandlerを指定しないと、Subscribeできない。
		// ただし、指定した場合でもretainされたメッセージは最初の1度しか取得されず、2回目以降のアクセスでは空になる点に注意。
		var result = &subscribeResult{}
		result.Count = 0

		var handler MQTT.MessageHandler = func(client *MQTT.Client, msg MQTT.Message) {
			result.Count++
			if debug {
				fmt.Printf("Received at defaultHandler : topic=%s, message=%s\n", msg.Topic(), msg.Payload())
			}
		}
		opts.SetDefaultPublishHandler(handler)

		defaultHandlerResults[id] = result
	}

	client := MQTT.NewClient(opts)
	token := client.Connect()

	if token.Wait() && token.Error() != nil {
		fmt.Printf("Connected error: %s\n", token.Error())
		return nil
	}

	return client
}

// 非同期でBrokerとの接続を切断する。
func asyncDisconnect(clients []*MQTT.Client) {
	wg := new(sync.WaitGroup)

	for _, client := range clients {
		wg.Add(1)
		go func(c *MQTT.Client) {
			defer wg.Done()
			disconnect(c)
		}(client)
	}

	wg.Wait()
}

// Brokerとの接続を切断する。
func disconnect(client *MQTT.Client) {
	client.Disconnect(10)
}

// ファイルの存在チェックを行う。
// ファイルが存在する場合はtrue、存在しない場合はfalseを返す。
//   filePath : 存在をチェックするファイルのパス
func fileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

func main() {
	broker := flag.String("broker", "tcp://{host}:{port}", "URI of MQTT broker (required)")
	action := flag.String("action", "p|pub or s|sub", "Publish or Subscribe or Subscribe(with publishing) (required)")
	qos := flag.Int("qos", 0, "MQTT QoS(0|1|2)")
	retain := flag.Bool("retain", false, "MQTT Retain")
	topic := flag.String("topic", baseTopic, "Base topic")
	username := flag.String("broker-username", "", "Username for connecting to the MQTT broker")
	password := flag.String("broker-password", "", "Password for connecting to the MQTT broker")
	tls := flag.String("tls", "", "TLS mode. 'server:certFile' or 'client:rootCAFile,clientCertFile,clientKeyFile'")
	clients := flag.Int("clients", 10, "Number of clients")
	count := flag.Int("count", 100, "Number of loops per client")
	size := flag.Int("size", 1024, "Message size per publish (byte)")
	useDefaultHandler := flag.Bool("support-unknown-received", false, "Using default messageHandler for a message that does not match any known subscriptions")
	preTime := flag.Int("pretime", 3000, "Pre wait time (ms)")
	intervalTime := flag.Int("intervaltime", 0, "Interval time per message (ms)")
	debug := flag.Bool("x", false, "Debug mode")

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
	var certConfig certConfig = nil
	if *tls == "" {
		// nil
	} else if strings.HasPrefix(*tls, "server:") {
		var strArray = strings.Split(*tls, "server:")
		serverCertFile := strings.TrimSpace(strArray[1])
		if fileExists(serverCertFile) == false {
			fmt.Printf("File is not found. : certFile -> %s\n", serverCertFile)
			return
		}

		certConfig = serverCertConfig{
			ServerCertFile: serverCertFile}
	} else if strings.HasPrefix(*tls, "client:") {
		var strArray = strings.Split(*tls, "client:")
		var configArray = strings.Split(strArray[1], ",")
		rootCAFile := strings.TrimSpace(configArray[0])
		clientCertFile := strings.TrimSpace(configArray[1])
		clientKeyFile := strings.TrimSpace(configArray[2])
		if fileExists(rootCAFile) == false {
			fmt.Printf("File is not found. : rootCAFile -> %s\n", rootCAFile)
			return
		}
		if fileExists(clientCertFile) == false {
			fmt.Printf("File is not found. : clientCertFile -> %s\n", clientCertFile)
			return
		}
		if fileExists(clientKeyFile) == false {
			fmt.Printf("File is not found. : clientKeyFile -> %s\n", clientKeyFile)
			return
		}

		certConfig = clientCertConfig{
			RootCAFile:     rootCAFile,
			ClientCertFile: clientCertFile,
			ClientKeyFile:  clientKeyFile}
	} else {
		// nil
	}

	execOpts := execOptions{}
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

	debug = *debug

	switch method {
	case "pub":
		execute(publishAllClient, execOpts)
	case "sub":
		execute(subscribeAllClient, execOpts)
	}
}
