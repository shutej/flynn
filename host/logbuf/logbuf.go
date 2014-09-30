package logbuf

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/lumberjack"
)

type Data struct {
	Stream    int      `json:"s"`
	Timestamp UnixTime `json:"t"`
	Message   string   `json:"m"`
}

type UnixTime struct{ time.Time }

func (t UnixTime) MarshalJSON() ([]byte, error) {
	return strconv.AppendInt(nil, t.UnixNano()/int64(time.Millisecond), 10), nil
}

func (t *UnixTime) UnmarshalJSON(data []byte) error {
	i, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return errors.New("logbuf: invalid timestamp")
	}
	t.Time = time.Unix(0, i*int64(time.Millisecond))
	return nil
}

func NewLog(l *lumberjack.Logger) *Log {
	if l == nil {
		l = &lumberjack.Logger{}
	}
	if l.MaxSize == 0 {
		l.MaxSize = 100 * lumberjack.Megabyte
	}
	log := &Log{
		l:         l,
		listeners: make(map[int]map[chan Data]struct{}),
		buf:       make(map[int]*Data),
	}
	return log
}

type Log struct {
	l         *lumberjack.Logger
	listeners map[int]map[chan Data]struct{}
	mtx       sync.RWMutex
	buf       map[int]*Data
}

func (l *Log) AddListener(stream int, ch chan Data) {
	l.mtx.Lock()
	if _, ok := l.listeners[stream]; !ok {
		l.listeners[stream] = make(map[chan Data]struct{})
	}
	l.listeners[stream][ch] = struct{}{}
	l.mtx.Unlock()
}

func (l *Log) RemoveListener(stream int, ch chan Data) {
	l.mtx.Lock()
	delete(l.listeners[stream], ch)
	l.mtx.Unlock()
}

func (l *Log) sendData(data Data) {
	l.mtx.RLock()
	defer l.mtx.RUnlock()
	for ch := range l.listeners[-1] {
		ch <- data
	}
	for ch := range l.listeners[data.Stream] {
		ch <- data
	}
}

// Transcribe log events to log file.
func (l *Log) watch(stream int) error {
	ch := make(chan Data)
	l.AddListener(stream, ch)
	defer l.RemoveListener(stream, ch)

	for {
		data, ok := <-ch
		if !ok {
			break
		}
		// TODO: buffer until full line
		// l.buf[stream] = &data
		if err := l.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// Watch stream for new log events and transmit them.
func (l *Log) Follow(stream int, r io.Reader) error {
	go l.watch(stream)
	data := Data{Stream: stream}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data.Timestamp = UnixTime{time.Now()}
			data.Message = string(buf[:n])

			l.sendData(data)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Write a log event to the logfile.
func (l *Log) Write(data Data) error {
	j := json.NewEncoder(l.l)
	if err := j.Encode(data); err != nil {
		return err
	}
	return nil
}

// Read old log lines from a logfile.
func (l *Log) Read(lines int, ch chan Data) error {
	name, _ := l.l.File() // TODO: stitch older files together
	if name == "" {
		close(ch)
		return nil // no file == no logs
	}

	f, err := os.Open(name)
	defer f.Close()
	if err != nil {
		return err
	}
	r := bufio.NewReader(f)

	// seek to line if needed
	if lines != 0 {
		// TODO
	}

	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			data := Data{}
			json.Unmarshal(line, &data)
			ch <- data
		}
		if err != io.EOF && err != nil {
			return err
		}
		if err == io.EOF {
			break
		}
	}
	close(ch) // send a close event so we know everything was read
	return nil
}

func (l *Log) Close() error {
	l.mtx.Lock()
	for _, stream := range l.listeners {
		for ch := range stream {
			close(ch)
		}
	}
	l.mtx.Unlock()
	return l.l.Close()
}
