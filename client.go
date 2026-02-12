package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type Config struct {
	LocalAddr  string `json:"localAddr"`
	RemoteAddr string `json:"remoteAddr"`
}

var config Config

func loadConfig() error {
	// First load from config file
	data, err := os.ReadFile("config.json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}

	// Override with environment variables if set
	if localAddr := os.Getenv("LOCAL_ADDR"); localAddr != "" {
		config.LocalAddr = localAddr
	}
	if remoteAddr := os.Getenv("REMOTE_ADDR"); remoteAddr != "" {
		config.RemoteAddr = remoteAddr
	}

	return nil
}

func handleClient(localConn net.Conn) {
	defer localConn.Close()
	log.Printf("[Client] New Connection...")

	// 1. 连接远程 WebSocket
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	wsConn, _, err := dialer.Dial(config.RemoteAddr, nil)
	if err != nil {
		log.Printf("[Client] Dial Failed to %v: %v", config.RemoteAddr, err)
		return
	}
	defer wsConn.Close()
	log.Printf("[Client] Dial Success: %v", config.RemoteAddr)

	// 【关键】启动心跳协程
	// 每 15 秒发送一个 Ping，告诉服务器别断开我
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// 发送 Ping 控制帧 (不需要 Base64，这是协议层的)
				if err := wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second)); err != nil {
					log.Printf("[Client] Ping Failed: %v", err)
					return // 发送失败意味着连接断了
				}
			case <-stopHeartbeat:
				return
			}
		}
	}()
	defer close(stopHeartbeat) // 连接关闭时停止心跳

	log.Printf("[Client] Tunnel Established.")
	errChan := make(chan error, 2)

	// --- 上行：本地 -> 远程 ---
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := localConn.Read(buf)
			if err != nil {
				log.Printf("[Client] Upstream Read Error: %v", err)
				errChan <- err
				return
			}
			log.Printf("[Client] Upstream: Read %d bytes from local connection", n)
			log.Printf("[Client] Upstream: Data preview (first 200 bytes): %s", string(buf[:min(n, 200)]))
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			log.Printf("[Client] Upstream: Encoded to %d bytes, sending to WebSocket", len(encoded))
			if err := wsConn.WriteMessage(websocket.TextMessage, []byte(encoded)); err != nil {
				log.Printf("[Client] Upstream: WebSocket Write Error: %v", err)
				errChan <- err
				return
			}
			log.Printf("[Client] Upstream: Successfully sent to WebSocket")
		}
	}()

	// --- 下行：远程 -> 本地 ---
	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				log.Printf("[Client] Downstream: WebSocket Read Error: %v", err)
				errChan <- err
				return
			}

			cleanMsg := strings.TrimSpace(string(msg))
			if len(cleanMsg) == 0 {
				log.Printf("[Client] Downstream: Received empty message, skipping")
				continue
			}

			log.Printf("[Client] Downstream: Received %d bytes from WebSocket", len(cleanMsg))
			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil {
				log.Printf("[Client] Downstream: Base64 Decode Error: %v", err)
				continue
			}
			log.Printf("[Client] Downstream: Decoded to %d bytes", len(rawBytes))
			log.Printf("[Client] Downstream: Data preview (first 200 bytes): %s", string(rawBytes[:min(len(rawBytes), 200)]))

			if _, err := localConn.Write(rawBytes); err != nil {
				log.Printf("[Client] Downstream: Local Write Error: %v", err)
				errChan <- err
				return
			}
			log.Printf("[Client] Downstream: Successfully wrote %d bytes to local connection", len(rawBytes))
		}
	}()

	err = <-errChan
	log.Printf("[Client] Connection Closed: %v", err)

	// Close connections to unblock the other goroutine
	localConn.Close()
	wsConn.Close()
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	listener, err := net.Listen("tcp", config.LocalAddr)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("== Heartbeat Client Started on %s ==", config.LocalAddr)

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		os.Exit(0)
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("Accept error:", err)
			continue
		}
		go handleClient(conn)
	}
}
