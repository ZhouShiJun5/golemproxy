// proxy listens on a socket and forwards data to one or more memcache servers (TODO: Actually shard requests)
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/TysonAndre/golemproxy/config"
	"github.com/TysonAndre/golemproxy/memcache"
	"github.com/TysonAndre/golemproxy/memcache/proxy/message"
	"github.com/TysonAndre/golemproxy/memcache/proxy/responsequeue"
	"github.com/TysonAndre/golemproxy/memcache/sharded"
	"go4.org/strutil"
)

var (
	space          = []byte(" ")
	requestAdd     = []byte("add")
	requestAppend  = []byte("append")
	requestDelete  = []byte("delete")
	requestGet     = []byte("get")
	requestGets    = []byte("gets")
	requestPrepend = []byte("prepend")
	requestReplace = []byte("replace")
	requestSet     = []byte("set")
)

const MAX_ITEM_SIZE = 1 << 20

// extractKeys extracts space-separated memcached keys from the bytes of a line
func extractKeys(line []byte) []string {
	parts := bytes.Split(line, space)
	result := make([]string, 0, len(line))
	for _, val := range parts {
		if len(val) == 0 {
			continue
		}
		result = append(result, string(val))
	}
	return result
}

// itob converts an integer to the bytes to represent that integer
func itob(value int) []byte {
	// TODO: optimize
	return []byte(strconv.Itoa(value))
}

func indexByteOffset(data []byte, c byte, offset int) int {
	for n := len(data); offset < n; offset++ {
		if data[offset] == c {
			return offset
		}
	}
	return -1
}

// handleGet forwards the 'get' or 'gets' (with CAS) request to a memcache client and sends a response back
// request is "get key1 key2 key3\r\n"
func handleGet(requestHeader []byte, responses *responsequeue.ResponseQueue, remote memcache.ClientInterface) error {
	// TODO: Check for malformed get command (e.g. stray \r)
	m := &message.SingleMessage{}

	keyI := bytes.IndexByte(requestHeader, ' ')
	if keyI < 0 {
		return errors.New("missing space")
	}
	nextKeyI := indexByteOffset(requestHeader, ' ', keyI+1)
	if nextKeyI < 0 {
		key := requestHeader[keyI+1 : len(requestHeader)-2]
		if len(key) == 0 {
			return errors.New("missing key")
		}
		// fmt.Fprintf(os.Stderr, "handleGet %q key=%v\n", string(requestHeader), string(key))
		m.HandleSendRequest(requestHeader, key, message.REQUEST_MC_GET)
		remote.SendProxiedMessageAsync(m)
		responses.RecordOutgoingRequest(m)
		return nil
	}
	return errors.New("multiget unsupported")
}

func handleDelete(requestHeader []byte, responses *responsequeue.ResponseQueue, remote memcache.ClientInterface) error {
	// TODO: Check for malformed delete command (e.g. stray \r)
	m := &message.SingleMessage{}

	keyI := bytes.IndexByte(requestHeader, ' ')
	if keyI < 0 {
		return errors.New("missing space")
	}
	nextKeyI := indexByteOffset(requestHeader, ' ', keyI+1)
	if nextKeyI < 0 {
		key := requestHeader[keyI+1 : len(requestHeader)-2]
		if len(key) == 0 {
			return errors.New("missing key")
		}
		m.HandleSendRequest(requestHeader, key, message.REQUEST_MC_DELETE)
		remote.SendProxiedMessageAsync(m)
		responses.RecordOutgoingRequest(m)
		return nil
	}
	return errors.New("delete does not support multiple keys")
}

func parseSetRequest(requestHeader []byte, reader *bufio.Reader) ([]byte, []byte, error) {
	// FIXME support 'noreply'
	// parse the number of bytes then read
	// set key <flags> <expiry> <valuelen> [noreply]\r\n<value>\r\n
	parts := bytes.Split(requestHeader[:len(requestHeader)-2], space)
	if len(parts) < 5 || len(parts) > 6 {
		return nil, nil, fmt.Errorf("unexpected word count %d for set, expected 'set key flags expiry valuelen [noreply]'", len(parts))
	}

	// TODO: use https://godoc.org/go4.org/strutil#ParseUintBytes
	_, err := strutil.ParseUintBytes(parts[2], 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse flags: %v", err)
	}
	_, err = strutil.ParseUintBytes(parts[3], 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse expiry: %v", err)
	}
	length, err := strutil.ParseUintBytes(parts[4], 10, 30)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse length: %v", err)
	}
	if length < 0 {
		return nil, nil, fmt.Errorf("Wrong length: expected non-negative value")
	}
	if length > MAX_ITEM_SIZE {
		return nil, nil, fmt.Errorf("Wrong length: %d exceeds MAX_ITEM_SIZE of %d", length, MAX_ITEM_SIZE)
	}
	fullRequestLength := len(requestHeader) + int(length) + 2
	bytes := make([]byte, fullRequestLength)
	copy(bytes, requestHeader)
	n, err := io.ReadFull(reader, bytes[len(requestHeader):])
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to read %d bytes, got %d: %v", length, n, err)
	}
	// skip \r\n
	if bytes[fullRequestLength-2] != '\r' || bytes[fullRequestLength-1] != '\n' {
		return nil, nil, fmt.Errorf("Value was not followed by \\r\\n")
	}
	return bytes, parts[1], nil

}

// handleSet forwards a set request to the memcache servers and returns a result.
// TODO: Add the capability to mock successful responses before sending the request
func handleSet(requestHeader []byte, reader *bufio.Reader, responses *responsequeue.ResponseQueue, remote memcache.ClientInterface) error {
	m := &message.SingleMessage{}
	requestBody, key, err := parseSetRequest(requestHeader, reader)
	if err != nil {
		return err
	}
	m.HandleSendRequest(requestBody, key, message.REQUEST_MC_SET)
	remote.SendProxiedMessageAsync(m)
	responses.RecordOutgoingRequest(m)
	return nil
}

func handleCommand(reader *bufio.Reader, responses *responsequeue.ResponseQueue, remote memcache.ClientInterface) error {
	// ReadBytes is safe to reuse, ReadSlice isn't.
	header, err := reader.ReadBytes('\n')
	if err != nil {
		// Check if the reader exited cleanly
		if err != io.EOF {
			// TODO: Handle EOF
			fmt.Fprintf(os.Stderr, "ReadSlice failed: %s\n", err.Error())
		}
		return err
	}
	if len(header) < 2 || header[len(header)-2] != '\r' {
		return errors.New("request header did not end with \\r\\n")
	}

	i := bytes.IndexByte(header, ' ')
	if i <= 1 {
		return errors.New("empty request")
	}

	// fmt.Fprintf(os.Stderr, "got request %q i=%d\n", header, i)
	switch i {
	case 3:
		// memcached protocol is case sensitive
		if bytes.HasPrefix(header, requestGet) {
			err := handleGet(header, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "get request parsing failed: %s\n", err.Error())
			}
			return err
		}
		if bytes.HasPrefix(header, requestSet) || bytes.HasPrefix(header, requestAdd) {
			err := handleSet(header, reader, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s request parsing failed: %s\n", string(header[:3]), err.Error())
			}
			return err
		}
	case 4:
		// memcached protocol is case sensitive
		if bytes.HasPrefix(header, requestGets) {
			err := handleGet(header, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gets request parsing failed: %s\n", err.Error())
			}
			return err
		}
	case 6:
		if bytes.HasPrefix(header, requestDelete) {
			err := handleDelete(header, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "delete request parsing failed: %s\n", err.Error())
			}
			return err
		}
		if bytes.HasPrefix(header, requestAppend) {
			err := handleSet(header, reader, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "append request parsing failed: %s\n", err.Error())
			}
			return err
		}
	case 7:
		if bytes.HasPrefix(header, requestReplace) || bytes.HasPrefix(header, requestPrepend) {
			err := handleDelete(header, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s request parsing failed: %s\n", string(header[:7]), err.Error())
			}
			return err
		}
		if bytes.HasPrefix(header, requestAppend) {
			err := handleSet(header, reader, responses, remote)
			if err != nil {
				fmt.Fprintf(os.Stderr, "append request parsing failed: %s\n", err.Error())
			}
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "Unknown command %q", header)
	return errors.New("unknown command")
}

// serveSocket runs in a loop to read memcached requests and send memcached responses
func serveSocket(remote memcache.ClientInterface, c net.Conn) {
	reader := bufio.NewReader(c)
	responseQueue := responsequeue.CreateResponseQueue(c)

	for {
		err := handleCommand(reader, responseQueue, remote)
		if err != nil {
			c.Close()
			return
		}
	}
}

func handleUnexpectedExit(listeners []net.Listener, didExit *bool) {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)
	go func(c chan os.Signal) {
		// Wait for a SIGINT or SIGKILL:
		sig := <-c
		*didExit = true
		fmt.Fprintf(os.Stderr, "Caught signal %s: shutting down.\n", sig)
		for _, l := range listeners {
			// Stop listening (and unlink the socket if unix type):
			l.Close()
		}
		// And we're done:
		os.Exit(0)
	}(sigc)
}

func createUnixSocket(path string) (net.Listener, error) {
	fmt.Fprintf(os.Stderr, "Listening for memcache requests at unix socket %q\n", path)
	l, err := net.Listen("unix", path)
	return l, err
}

func createTCPSocket(path string) (net.Listener, error) {
	fmt.Fprintf(os.Stderr, "Listening for memcache requests at tcp server %q\n", path)
	l, err := net.Listen("tcp", path)
	return l, err
}

func serveSocketServer(remote memcache.ClientInterface, l net.Listener, path string, didExit *bool) {
	for {
		fd, err := l.Accept()
		if *didExit {
			return
		}
		if err != nil {
			// TODO: Clean up debug code
			fmt.Fprintf(os.Stderr, "accept error for %q: %v", path, err)
			return
		}

		go serveSocket(remote, fd)
	}
}

func Run(configs map[string]config.Config) {
	var wg sync.WaitGroup
	wg.Add(len(configs))

	didExit := false
	listeners := []net.Listener{}

	for _, config := range configs {
		remote := sharded.New(config)
		socketPath := config.Listen
		// TODO: Also support tcp sockets
		var l net.Listener
		var err error
		i := strings.IndexRune(socketPath, ':')
		if i >= 0 {
			l, err = createTCPSocket(socketPath)
		} else {
			l, err = createUnixSocket(socketPath)
		}
		if err != nil {
			// TODO: Clean up the rest of the sockets
			fmt.Fprintf(os.Stderr, "Listen error at %s: %v\n", socketPath, err)
			for _, l := range listeners {
				l.Close()
			}
			return
		}
		listeners = append(listeners, l)

		go func() {
			defer l.Close()
			serveSocketServer(remote, l, socketPath, &didExit)
			wg.Done()
		}()
	}
	handleUnexpectedExit(listeners, &didExit)
	wg.Wait()
}
