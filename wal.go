package wal

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
)

type WALOptions struct {
	// LogDir is where the wal logs will be stored
	LogDir string

	// Maximum size in bytes for each file
	MaxLogSize int64

	// The entire wal is broken down into smaller segments.
	// This will be helpful during log rotation and management
	// maximum number of log segments
	MaxSegments int

	log *zap.Logger
}

type WriteAheadLog struct {
	logFileName  string
	file         *os.File
	mu           sync.Mutex
	maxLogSize   int64
	logSize      int64
	segmentCount int

	maxSegments      int
	currentSegmentID int

	log *zap.Logger
}

func NewWriteAheadLog(opts *WALOptions) (*WriteAheadLog, error) {
	walLogFilePrefix := opts.LogDir + "wal"

	firstLogFileName := walLogFilePrefix + ".0"
	file, err := os.OpenFile(firstLogFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}

	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}

	return &WriteAheadLog{
		logFileName: walLogFilePrefix,
		file:        file,

		maxLogSize: opts.MaxLogSize,
		logSize:    fi.Size(),

		segmentCount: 0,

		maxSegments:      opts.MaxSegments,
		currentSegmentID: 0,
		log:              opts.log,
	}, nil
}

func (wal *WriteAheadLog) Write(data []byte) (int64, error) {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	entrySize := 4 + len(data) // 4 bytes for the size prefix
	if wal.logSize+int64(entrySize) > wal.maxLogSize {
		if err := wal.rotateLog(); err != nil {
			return 0, err
		}
	}

	offset, err := wal.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	// Create a buffer to hold the log entry data
	buf := make([]byte, 4+len(data))

	// Write the size prefix to the buffer
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(data)))

	// Copy the data payload to the buffer
	copy(buf[8:], data)

	// Write the entire buffer to the file in a single system call
	if _, err := wal.file.Write(buf); err != nil {
		return 0, err
	}

	wal.logSize += int64(entrySize)
	return offset, nil
}

func (wal *WriteAheadLog) Close() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	return wal.file.Close()
}

func (wal *WriteAheadLog) GetOffset() (int64, error) {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	return wal.file.Seek(0, io.SeekEnd)
}

func (wal *WriteAheadLog) rotateLog() error {
	if err := wal.file.Close(); err != nil {
		return err
	}

	if wal.segmentCount >= wal.maxSegments {
		if err := wal.deleteOldestSegment(); err != nil {
			return err
		}

	}
	wal.currentSegmentID++
	wal.segmentCount++

	newFileName := fmt.Sprintf("%s.%d", wal.logFileName, wal.currentSegmentID)

	file, err := os.OpenFile(newFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return err
	}

	wal.file = file
	wal.logSize = 0
	return nil
}

func (wal *WriteAheadLog) deleteOldestSegment() error {
	oldestSegment := fmt.Sprintf("%s.%d", wal.logFileName, wal.currentSegmentID-wal.maxSegments)

	wal.log.Info("Removing wal file", zap.String("segment", oldestSegment))
	if err := os.Remove(oldestSegment); err != nil {
		return err
	}

	// Update the segment count
	wal.segmentCount--

	return nil
}

func (wal *WriteAheadLog) Replay(offset int64, f func([]byte) error) error {
	logFiles, err := filepath.Glob(wal.logFileName + "*")
	if err != nil {
		return err
	}

	for _, logFile := range logFiles {
		file, err := os.Open(logFile)
		if err != nil {
			return err
		}
		defer file.Close()

		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return err
		}

		var sizeBuf [4]byte

		for {
			_, err := io.ReadFull(file, sizeBuf[:])
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}

			size := binary.LittleEndian.Uint32(sizeBuf[:])
			data := make([]byte, size)
			_, err = io.ReadFull(file, data)
			if err != nil {
				return err
			}

			if err := f(data); err != nil {
				return err
			}
		}
	}

	return nil
}