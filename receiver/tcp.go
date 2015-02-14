package receiver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"devroom.ru/lomik/carbon/points"

	"github.com/Sirupsen/logrus"
	"github.com/hydrogen18/stalecucumber"
)

// TCP receive metrics from TCP connections
type TCP struct {
	out             chan *points.Points
	exit            chan bool
	graphPrefix     string
	metricsReceived uint32
	errors          uint32
	active          int32 // counter
	listener        *net.TCPListener
	isPickle        bool
}

// NewTCP create new instance of TCP
func NewTCP(out chan *points.Points) *TCP {
	return &TCP{
		out:      out,
		exit:     make(chan bool),
		isPickle: false,
	}
}

// NewPickle create new instance of TCP with pickle listener enabled
func NewPickle(out chan *points.Points) *TCP {
	return &TCP{
		out:      out,
		exit:     make(chan bool),
		isPickle: true,
	}
}

// SetGraphPrefix for internal cache metrics
func (rcv *TCP) SetGraphPrefix(prefix string) {
	rcv.graphPrefix = prefix
}

// Stat sends internal statistics to cache
func (rcv *TCP) Stat(metric string, value float64) {
	var protocolPrefix string

	if rcv.isPickle {
		protocolPrefix = "pickle"
	} else {
		protocolPrefix = "tcp"
	}

	rcv.out <- points.OnePoint(
		fmt.Sprintf("%s%s.%s", rcv.graphPrefix, protocolPrefix, metric),
		value,
		time.Now().Unix(),
	)
}

// Addr returns binded socket address. For bind port 0 in tests
func (rcv *TCP) Addr() net.Addr {
	if rcv.listener == nil {
		return nil
	}
	return rcv.listener.Addr()
}

func (rcv *TCP) handleConnection(conn net.Conn) {
	atomic.AddInt32(&rcv.active, 1)
	defer atomic.AddInt32(&rcv.active, -1)

	defer conn.Close()
	reader := bufio.NewReader(conn)

	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))

		line, err := reader.ReadBytes('\n')

		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					logrus.Warningf("[tcp] Unfinished line: %#v", line)
				}
			} else {
				atomic.AddUint32(&rcv.errors, 1)
				logrus.Error(err)
			}
			break
		}
		if len(line) > 0 { // skip empty lines
			if msg, err := points.ParseText(string(line)); err != nil {
				atomic.AddUint32(&rcv.errors, 1)
				logrus.Info(err)
			} else {
				atomic.AddUint32(&rcv.metricsReceived, 1)
				rcv.out <- msg
			}
		}
	}
}

func (rcv *TCP) handlePickle(conn net.Conn) {
	atomic.AddInt32(&rcv.active, 1)
	defer atomic.AddInt32(&rcv.active, -1)

	defer conn.Close()
	reader := bufio.NewReader(conn)

	var msgLen uint32
	var err error

	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))

		err = binary.Read(reader, binary.BigEndian, &msgLen)
		if err != nil {
			if err == io.EOF {
				return
			}

			atomic.AddUint32(&rcv.errors, 1)
			logrus.Warningf("[pickle] Can't read message length: %s", err.Error())
			return
		}

		data := make([]byte, msgLen)

		if err = binary.Read(reader, binary.BigEndian, data); err != nil {
			atomic.AddUint32(&rcv.errors, 1)
			logrus.Warningf("[pickle] Can't read message body: %s", err.Error())
			return
		}

		result, err := stalecucumber.Unpickle(bytes.NewReader(data))
		if err != nil {
			atomic.AddUint32(&rcv.errors, 1)
			logrus.Warningf("[pickle] Can't unpickle message: %s", err.Error())
			return
		}

		list, err := stalecucumber.ListOrTuple(result, err)
		if err != nil {
			atomic.AddUint32(&rcv.errors, 1)
			logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
			return
		}

		for i := 0; i < len(list); i++ {
			metric, err := stalecucumber.ListOrTuple(list[i], nil)
			if err != nil {
				atomic.AddUint32(&rcv.errors, 1)
				logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
				return
			}

			if len(metric) < 2 {
				atomic.AddUint32(&rcv.errors, 1)
				logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
				return
			}

			name, err := stalecucumber.String(metric[0], nil)
			if err != nil {
				atomic.AddUint32(&rcv.errors, 1)
				logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
				return
			}

			msg := points.New()
			msg.Metric = name

			for j := 1; j < len(metric); j++ {
				v, err := stalecucumber.ListOrTuple(metric[j], nil)
				if err != nil {
					atomic.AddUint32(&rcv.errors, 1)
					logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
					return
				}
				if len(v) != 2 {
					atomic.AddUint32(&rcv.errors, 1)
					logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
					return
				}
				timestamp, err := stalecucumber.Int(v[0], nil)
				if err != nil {
					atomic.AddUint32(&rcv.errors, 1)
					logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
					return
				}

				value, err := stalecucumber.Float(v[1], nil)
				if err != nil {
					valueInt, err := stalecucumber.Int(v[1], nil)
					if err != nil {
						atomic.AddUint32(&rcv.errors, 1)
						logrus.Warningf("[pickle] Wrong unpickled message: %s", err.Error())
						return
					}
					value = float64(valueInt)
				}

				msg.Add(value, timestamp)
			}

			atomic.AddUint32(&rcv.metricsReceived, 1)
			rcv.out <- msg
		}
	}
}

// Listen bind port. Receive messages and send to out channel
func (rcv *TCP) Listen(addr *net.TCPAddr) error {
	var err error
	rcv.listener, err = net.ListenTCP("tcp", addr)
	if err != nil {
		return err
	}

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				cnt := atomic.LoadUint32(&rcv.metricsReceived)
				atomic.AddUint32(&rcv.metricsReceived, -cnt)
				rcv.Stat("metricsReceived", float64(cnt))

				active := atomic.LoadInt32(&rcv.active)
				atomic.AddInt32(&rcv.active, -active)
				rcv.Stat("active", float64(active))

				errors := atomic.LoadUint32(&rcv.errors)
				atomic.AddUint32(&rcv.errors, -errors)
				rcv.Stat("errors", float64(errors))
			case <-rcv.exit:
				rcv.listener.Close()
				return
			}
		}
	}()

	handler := rcv.handleConnection
	if rcv.isPickle {
		handler = rcv.handlePickle
	}

	go func() {
		defer rcv.listener.Close()

		for {

			conn, err := rcv.listener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					break
				}
				logrus.Warningf("[tcp] Failed to accept connection: %s", err)
				continue
			}

			go handler(conn)
		}

	}()

	return nil
}

// Stop all listeners
func (rcv *TCP) Stop() {
	close(rcv.exit)
}