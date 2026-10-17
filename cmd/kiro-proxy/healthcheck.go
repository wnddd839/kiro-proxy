package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// probeHealth 请求本机 /health 并在非 2xx 时返回错误。
//
// 容器镜像里没有 curl，编排系统又需要一个可执行的健康检查，所以用二进制自己做。
// 它只做一次短超时的本机请求，不加载账号、不启动服务。
func probeHealth(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/health"
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("healthcheck: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
