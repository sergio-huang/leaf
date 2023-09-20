package network

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

type listener struct {
	Addr     string `json:"addr"`
	FD       int    `json:"fd"`
	Filename string `json:"filename"`
}

func importListener(addr string) (net.Listener, error) {
	// 从系统中获取编码后的监听端口元数据
	listenerEnv := os.Getenv("LISTENER")
	if listenerEnv == "" {
		return nil, fmt.Errorf("unable to find LISTENER environment variable")
	}

	// Unmarshal the listener metadata.
	// 解析元数据
	var l listener
	err := json.Unmarshal([]byte(listenerEnv), &l)
	if err != nil {
		return nil, err
	}
	if l.Addr != addr {
		return nil, fmt.Errorf("unable to find listener for %v", addr)
	}

	// 通过额外的元数据文件，重建端口监听
	listenerFile := os.NewFile(uintptr(l.FD), l.Filename)
	if listenerFile == nil {
		return nil, fmt.Errorf("unable to create listener file: %v", err)
	}
	defer listenerFile.Close()

	// Create a net.Listener from the *os.File.
	ln, err := net.FileListener(listenerFile)
	if err != nil {
		return nil, err
	}

	return ln, nil
}

func createListener(addr string) (net.Listener, error) {
	// 启动TCP监听服务连接
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 此处会返回监听失败信息
		return nil, err
	}

	return ln, nil
}

func createOrImportListener(addr string) (net.Listener, error) {
	// Try and import a listener for addr. If it's found, use it.
	ln, err := importListener(addr)
	if err == nil {
		fmt.Printf("Imported listener file descriptor for %v.\n", addr)
		return ln, nil
	}

	// 当端口启动失败，证明这个端口已经被占用
	ln, err = createListener(addr)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Created listener file descriptor for %v.\n", addr)
	return ln, nil
}

func getListenerFile(ln net.Listener) (*os.File, error) {
	switch t := ln.(type) {
	case *net.TCPListener:
		return t.File()
	case *net.UnixListener:
		return t.File()
	}
	return nil, fmt.Errorf("unsupported listener: %T", ln)
}

func forkChild(addr string, ln net.Listener) (*os.Process, error) {
	// 获取端口和元数据放在子进程中。
	lnFile, err := getListenerFile(ln)
	if err != nil {
		return nil, err
	}
	defer lnFile.Close()
	// 获取端口
	l := listener{
		Addr:     addr,
		FD:       3,
		Filename: lnFile.Name(),
	}
	listenerEnv, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}

	// Pass stdin, stdout, and stderr along with the listener to the child.
	// 获取系统输入输出流文件
	files := []*os.File{
		os.Stdin,
		os.Stdout,
		os.Stderr,
		lnFile,
	}
	// Get current environment and add in the listener to it.
	// 获取环境和添加端口
	environment := append(os.Environ(), "LISTENER="+string(listenerEnv))

	// 获取进程名称和文件
	execName, err := os.Executable()
	if err != nil {
		return nil, err
	}
	execDir := filepath.Dir(execName)

	// 开个子进程
	p, err := os.StartProcess(execName, []string{execName}, &os.ProcAttr{
		Dir:   execDir,
		Env:   environment,
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})
	if err != nil {
		return nil, err
	}
	// 返回系统进程
	return p, nil
}

func waitForSignals(addr string, ln net.Listener) {
	// 到底用多少缓冲合适呢？需要根据自己的服务大小？我觉得不需要，于是用1
	//signalCh := make(chan os.Signal, 1024)
	signalCh := make(chan os.Signal, 1)
	// 此处可以接收很多种信息，本例子主要是接收SIGHUP信号，从而fork一个进程
	// SIGHUP终止收到该信号的进程，用于重启。
	// SIGINT强制结束进程
	// SIGQUIT结束进程和dump core
	signal.Notify(signalCh, syscall.SIGHUP, syscall.SIGUSR2, syscall.SIGINT, syscall.SIGQUIT)
	// 出于保护机制，选择for、select、case来进行 读取channel。因为case可以保护channel在panic情况下不报错
	for {
		select {
		case s := <-signalCh:
			fmt.Printf("%v 信号接收.\n", s)
			switch s {

			case syscall.SIGHUP:
				// fork一个子分支进程，保障运行后，再去关闭服务。即使有服务进来，也不会受到影响，依然运行。
				p, err := forkChild(addr, ln)
				if err != nil {
					fmt.Printf("fork子分支失败: %v.\n", err)
					continue
				}
				fmt.Printf("Forked child子分支Pid: %v.\n", p.Pid)
				ln.Close()
			case syscall.SIGUSR2:
				// fork一个子分支进程.
				p, err := forkChild(addr, ln)
				if err != nil {
					fmt.Printf("fork子分支失败: %v.\n", err)
					continue
				}

				// 打印这个PID，等待更多信号
				fmt.Printf("Forked child %v.\n", p.Pid)
			case syscall.SIGINT, syscall.SIGQUIT:
				// 创建一个上下文，当关机时，超过5秒算是超时。
				ln.Close()
				fmt.Printf("SIGINT.\n")
			}
		}
	}
}
