package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/klog/v2"
)

var (
	kubeconfig string
	namespace  string
	name       string
	port       int
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&namespace, "namespace", "", "Namespace of the target Pod")
	flag.StringVar(&name, "name", "", "Name of the target Pod")
	flag.IntVar(&port, "port", 80, "Port to forward")
	klog.InitFlags(nil)
	flag.Parse()
}

type Proxier struct {
	streamConn    httpstream.Connection
	port          int
	requestIDLock sync.Mutex
	requestID     int
}

func newPodProexier(name, namespace string, config *rest.Config, clientset *kubernetes.Clientset, port int) (*Proxier, error) {
	dialer, err := newPodDialer(config, clientset, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to init dialer: %v", err)
	}
	streamConn, addr, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %v", addr, err)
	}
	return &Proxier{
		streamConn:    streamConn,
		port:          port,
		requestIDLock: sync.Mutex{},
		requestID:     1,
	}, nil
}

func (p *Proxier) Start() error {
	defer p.streamConn.Close()
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, fmt.Sprintf("%d", p.port))
	headers.Set(corev1.PortForwardRequestIDHeader, strconv.Itoa(p.getRequestId()))

	// error stream
	errorStream, err := p.streamConn.CreateStream(headers)
	if err != nil {
		return fmt.Errorf("failed to create error stream for port %d: %v", p.port, err)
	}
	errorStream.Close()
	errorChan := make(chan error)
	go func() {
		message, err := ioutil.ReadAll(errorStream)
		switch {
		case err != nil:
			errorChan <- fmt.Errorf("failed to read from error stream: %v", err)
		case len(message) > 0:
			errorChan <- fmt.Errorf("an error occurred forwarding: %v", string(message))
		}
		close(errorChan)
	}()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := p.streamConn.CreateStream(headers)
	if err != nil {
		return fmt.Errorf("failed to create data stream for port %d: %v", p.port, err)
	}

	remoteDone := make(chan struct{})
	localError := make(chan struct{})

	// copy from dataStream to stdout
	go func() {
		if _, err := io.Copy(os.Stdout, dataStream); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			klog.Error("error copying from remote stream to stdout: %v", err)
		}
		close(remoteDone)
	}()
	// copy from stdint to dataStream
	go func() {
		defer dataStream.Close()
		if _, err := io.Copy(dataStream, os.Stdin); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			klog.Error("error copying from stdin to remote stream: %v", err)
			// break out of the select below without waiting for the other copy to finish
			close(localError)
		}
	}()

	select {
	case <-remoteDone:
	case <-localError:
	case <-p.streamConn.CloseChan():
	}

	// always expect something on errorChan (it may be nil)
	if err := <-errorChan; err != nil {
		return err
	}
	return nil
}

func (p *Proxier) getRequestId() int {
	p.requestIDLock.Lock()
	defer p.requestIDLock.Unlock()
	p.requestID++
	return p.requestID
}

func newPodDialer(config *rest.Config, clientset *kubernetes.Clientset, ns, pod string) (httpstream.Dialer, error) {
	url := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(pod).
		SubResource("portforward").URL()

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create spdy round tripper: %v", err)
	}

	return spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url), nil
}

func main() {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to build client config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to init kube client: %v", err)
	}

	p, err := newPodProexier(name, namespace, config, clientset, port)
	if err != nil {
		klog.Fatalf("Failed to init proxier: %v", err)
	}
	if err := p.Start(); err != nil {
		klog.Fatalf("Failed to proxy: %v", err)
	}
}
