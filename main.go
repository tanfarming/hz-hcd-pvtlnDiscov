package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"encoding/json"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type key int

const (
	requestIDKey key = 0
)

var (
	listenAddr string
	healthy    int32

	logger       = log.New(os.Stdout, "http: ", log.LstdFlags)
	zonalNameMap = make(map[string]string)
)

func main() {

	initialize()
	smokeTest()

	router := http.NewServeMux()
	router.Handle("/", root())
	router.Handle("/healthz", healthz())
	router.Handle("/cluster/discovery", discovery())

	startServer(router, 5700)

}

func root() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, " hi. try /healthz or /cluster/discovery ")
	})
}

func healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

func discovery() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getMap())
	})
}

//-------------------------------
func initialize() {
	err := json.Unmarshal([]byte(os.Getenv("zonalNameMap")), &zonalNameMap)
	if err != nil {
		logger.Panicf("FAILED TO Unmarshal zonalNameMap. Err: %v", err)
	}
}

func smokeTest() {
	clientSet := getLocalK8sClientSet()

	nodeList, err := clientSet.CoreV1().Nodes().List(metav1.ListOptions{})
	nodes := nodeList.Items
	if err != nil || len(nodes) < 1 {
		logger.Panicf("FAILED TO GET NODES. Err: %v", err)
	}

	podList, err := clientSet.CoreV1().Pods(os.Getenv("imdgNamespace")).List(metav1.ListOptions{LabelSelector: os.Getenv("imdgPodSelector")})
	pods := podList.Items
	if err != nil || len(pods) < 1 {
		logger.Panicf("FAILED TO GET IMDG PODS. Err: %v", err)
	}
}

func startServer(router *http.ServeMux, port int) {
	flag.StringVar(&listenAddr, "listen-addr", ":"+strconv.Itoa(port), "server listen address")
	flag.Parse()

	logger.Println("Server is starting...")

	nextRequestID := func() string {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      tracing(nextRequestID)(logging(logger)(router)),
		ErrorLog:     logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	go func() {
		<-quit
		logger.Println("Server is shutting down...")
		atomic.StoreInt32(&healthy, 0)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			logger.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		close(done)
	}()
	logger.Println("Server is ready to handle requests at", listenAddr)
	atomic.StoreInt32(&healthy, 1)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("Could not listen on %s: %v\n", listenAddr, err)
	}
	<-done
	logger.Println("Server stopped")
}

func logging(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				requestID, ok := r.Context().Value(requestIDKey).(string)
				if !ok {
					requestID = "unknown"
				}
				logger.Println(requestID, r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func tracing(nextRequestID func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-Id")
			if requestID == "" {
				requestID = nextRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			w.Header().Set("X-Request-Id", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func getMap() interface{} {

	// config, err := rest.InClusterConfig()
	// if err != nil {
	// 	panic(fmt.Sprintf("Error occurred while creating kubernetes config. Err: %v", err))
	// }
	// clientSet, err := kubernetes.NewForConfig(config)
	// if err != nil {
	// 	panic(fmt.Sprintf("Error occurred while connecting to kubernetes cluster. Err: %v", err))
	// }

	clientSet := getLocalK8sClientSet()

	nodeIP_zone_map := make(map[string]string)

	nodeList, _ := clientSet.CoreV1().Nodes().List(metav1.ListOptions{})
	nodes := nodeList.Items
	for _, node := range nodes {
		nodeIP_zone_map[node.Status.Addresses[0].Address] = node.Labels["failure-domain.beta.kubernetes.io/zone"]
	}

	podList, _ := clientSet.CoreV1().Pods(os.Getenv("imdgNamespace")).List(metav1.ListOptions{LabelSelector: os.Getenv("imdgPodSelector")})
	pods := podList.Items

	// podIPmap := make(map[string]string)
	podIPkey := os.Getenv("PRIVATE_ADDRESS_PROPERTY")
	if podIPkey == "" {
		podIPkey = "private-address"
	}
	zonalNameKey := os.Getenv("PUBLIC_ADDRESS_PROPERTY")
	if zonalNameKey == "" {
		zonalNameKey = "public-address"
	}

	fatMap := make([]map[string]string, 0, 0)
	for _, pod := range pods {
		zone := nodeIP_zone_map[pod.Status.HostIP]
		ipNport := pod.Status.PodIP + ":5701"
		pvtlnNport := zonalNameMap[zone] + ":" + getPort(pod.Status.PodIP)
		// podIPmap[ipNport] = pvtlnNport
		var buffer = make(map[string]string)
		buffer[podIPkey] = ipNport
		buffer[zonalNameKey] = pvtlnNport
		fatMap = append(fatMap, buffer)
	}
	//return podIPmap
	return fatMap
}

func getPort(ip string) string {
	d := strings.Split(ip, ".")
	ipD3, _ := strconv.Atoi(d[2])
	ipD4, _ := strconv.Atoi(d[3])
	return strconv.Itoa(ipD3*256 + ipD4)
}

func getLocalK8sClientSet() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(fmt.Sprintf("Error occurred while creating kubernetes config. Err: %v", err))
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("Error occurred while connecting to kubernetes cluster. Err: %v", err))
	}
	return clientSet
}
