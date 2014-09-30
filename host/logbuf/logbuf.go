package logbuf

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/ActiveState/tail"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/natefinch/lumberjack"
	"github.com/flynn/flynn/pkg/random"
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
		l.MaxSize = 100 // megabytes
	}
	if l.Filename == "" {
		l.Filename = path.Join(os.TempDir(), random.String(16)+".log")
	}
	l.Rotate() // force creating a log file straight away
	log := &Log{
		l:   l,
		buf: make(map[int]*Data),
	}
	return log
}

type Log struct {
	l   *lumberjack.Logger
	buf map[int]*Data
}

// Watch stream for new log events and transmit them.
func (l *Log) Follow(stream int, r io.Reader) error {
	data := Data{Stream: stream}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data.Timestamp = UnixTime{time.Now()}
			data.Message = string(buf[:n])

			l.Write(data)
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
	return json.NewEncoder(l.l).Encode(data)
}

// Read old log lines from a logfile.
func (l *Log) Read(lines uint, follow bool, ch chan Data) error {
	name := l.l.Filename // TODO: stitch older files together

	f, err := os.Open(name)
	defer f.Close()
	if err != nil {
		return err
	}

	// seek to line if needed
	var seek int64
	if lines != 0 {
		blockSize := 512
		block := -1
		size, err := f.Seek(0, os.SEEK_END)
		if err != nil {
			return err
		}
		buf := make([]byte, blockSize)
		count := 0
		for {
			step := int64(block * blockSize)
			pos := size + step
			if pos < 0 {
				pos = 0
			}

			f.Seek(pos, os.SEEK_SET)
			if _, err := f.Read(buf); err != nil {
				return err
			}
			count += bytes.Count(buf, []byte("\n"))
			if count >= int(lines+1) { // looking for the newline before our first line
				diff := count - int(lines+1)
				lastpos := 0
				for diff >= 0 {
					lastpos += bytes.Index(buf[lastpos:], []byte("\n")) + 1
					diff--
				}
				seek = pos + int64(lastpos)
				break
			}
			if pos == 0 { // less lines in entire file, return everything
				seek = 0
				break
			}
			block--
		}
	}

	t, err := tail.TailFile(name, tail.Config{
		Follow: follow,
		ReOpen: follow,
		Location: &tail.SeekInfo{
			Offset: seek,
			Whence: os.SEEK_SET,
		},
	})
	for line := range t.Lines {
		data := Data{}
		if err := json.Unmarshal([]byte(line.Text), &data); err != nil {
			return err
		}
		ch <- data
	}
	close(ch) // send a close event so we know everything was read
	return nil
}

func (l *Log) Close() error {
	return l.l.Close()
}
