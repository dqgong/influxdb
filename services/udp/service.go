// Package udp provides the UDP input service for InfluxDB.
package udp // import "github.com/influxdata/influxdb/services/udp"

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/tsdb"
)

const (
	// Arbitrary, testing indicated that this doesn't typically get over 10
	parserChanLen = 1000

	// MaxUDPPayload is largest payload size the UDP service will accept.
	MaxUDPPayload = 64 * 1024
)

// statistics gathered by the UDP package.
const (
	statPointsReceived      = "pointsRx"
	statBytesReceived       = "bytesRx"
	statPointsParseFail     = "pointsParseFail"
	statReadFail            = "readFail"
	statBatchesTransmitted  = "batchesTx"
	statPointsTransmitted   = "pointsTx"
	statBatchesTransmitFail = "batchesTxFail"
)

type Diagnostic interface {
	Started(bindAddress string)
	Closed()
	CreateInternalStorageFailure(db string, err error)
	PointWriterError(database string, err error)
	ParseError(err error)
	ReadFromError(err error)
}

// Service is a UDP service that will listen for incoming packets of line protocol.
type Service struct {
	conn *net.UDPConn
	addr *net.UDPAddr
	wg   sync.WaitGroup

	mu    sync.RWMutex
	ready bool          // Has the required database been created?
	done  chan struct{} // Is the service closing or closed?

	parserChan chan []byte
	batcher    *tsdb.PointBatcher
	config     Config

	PointsWriter interface {
		WritePointsPrivileged(database, retentionPolicy string, consistencyLevel models.ConsistencyLevel, points []models.Point) error
	}

	MetaClient interface {
		CreateDatabase(name string) (*meta.DatabaseInfo, error)
	}

	Diagnostic  Diagnostic
	stats       *Statistics
	defaultTags models.StatisticTags
}

// NewService returns a new instance of Service.
func NewService(c Config) *Service {
	d := *c.WithDefaults()
	return &Service{
		config:      d,
		parserChan:  make(chan []byte, parserChanLen),
		batcher:     tsdb.NewPointBatcher(d.BatchSize, d.BatchPending, time.Duration(d.BatchTimeout)),
		stats:       &Statistics{},
		defaultTags: models.StatisticTags{"bind": d.BindAddress},
	}
}

// Open starts the service.
func (s *Service) Open() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed() {
		return nil // Already open.
	}
	s.done = make(chan struct{})

	if s.config.BindAddress == "" {
		return errors.New("bind address has to be specified in config")
	}
	if s.config.Database == "" {
		return errors.New("database has to be specified in config")
	}

	s.addr, err = net.ResolveUDPAddr("udp", s.config.BindAddress)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address %s: %s", s.config.BindAddress, err)
	}

	s.conn, err = net.ListenUDP("udp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to set up UDP listener at address %s: %s", s.addr, err)
	}

	if s.config.ReadBuffer != 0 {
		err = s.conn.SetReadBuffer(s.config.ReadBuffer)
		if err != nil {
			return fmt.Errorf("failed to set UDP read buffer to %d: %s",
				s.config.ReadBuffer, err)
		}
	}

	if s.Diagnostic != nil {
		s.Diagnostic.Started(s.config.BindAddress)
	}

	s.wg.Add(3)
	go s.serve()
	go s.parser()
	go s.writer()

	return nil
}

// Statistics maintains statistics for the UDP service.
type Statistics struct {
	PointsReceived      int64
	BytesReceived       int64
	PointsParseFail     int64
	ReadFail            int64
	BatchesTransmitted  int64
	PointsTransmitted   int64
	BatchesTransmitFail int64
}

// Statistics returns statistics for periodic monitoring.
func (s *Service) Statistics(tags map[string]string) []models.Statistic {
	return []models.Statistic{{
		Name: "udp",
		Tags: s.defaultTags.Merge(tags),
		Values: map[string]interface{}{
			statPointsReceived:      atomic.LoadInt64(&s.stats.PointsReceived),
			statBytesReceived:       atomic.LoadInt64(&s.stats.BytesReceived),
			statPointsParseFail:     atomic.LoadInt64(&s.stats.PointsParseFail),
			statReadFail:            atomic.LoadInt64(&s.stats.ReadFail),
			statBatchesTransmitted:  atomic.LoadInt64(&s.stats.BatchesTransmitted),
			statPointsTransmitted:   atomic.LoadInt64(&s.stats.PointsTransmitted),
			statBatchesTransmitFail: atomic.LoadInt64(&s.stats.BatchesTransmitFail),
		},
	}}
}

func (s *Service) writer() {
	defer s.wg.Done()

	for {
		select {
		case batch := <-s.batcher.Out():
			// Will attempt to create database if not yet created.
			if err := s.createInternalStorage(); err != nil {
				if s.Diagnostic != nil {
					s.Diagnostic.CreateInternalStorageFailure(s.config.Database, err)
				}
				continue
			}

			if err := s.PointsWriter.WritePointsPrivileged(s.config.Database, s.config.RetentionPolicy, models.ConsistencyLevelAny, batch); err == nil {
				atomic.AddInt64(&s.stats.BatchesTransmitted, 1)
				atomic.AddInt64(&s.stats.PointsTransmitted, int64(len(batch)))
			} else {
				if s.Diagnostic != nil {
					s.Diagnostic.PointWriterError(s.config.Database, err)
				}
				atomic.AddInt64(&s.stats.BatchesTransmitFail, 1)
			}

		case <-s.done:
			return
		}
	}
}

func (s *Service) serve() {
	defer s.wg.Done()

	buf := make([]byte, MaxUDPPayload)
	s.batcher.Start()
	for {
		select {
		case <-s.done:
			// We closed the connection, time to go.
			return
		default:
			// Keep processing.
			n, _, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				atomic.AddInt64(&s.stats.ReadFail, 1)
				if s.Diagnostic != nil {
					s.Diagnostic.ReadFromError(err)
				}
				continue
			}
			atomic.AddInt64(&s.stats.BytesReceived, int64(n))

			bufCopy := make([]byte, n)
			copy(bufCopy, buf[:n])
			s.parserChan <- bufCopy
		}
	}
}

func (s *Service) parser() {
	defer s.wg.Done()

	for {
		select {
		case <-s.done:
			return
		case buf := <-s.parserChan:
			points, err := models.ParsePointsWithPrecision(buf, time.Now().UTC(), s.config.Precision)
			if err != nil {
				atomic.AddInt64(&s.stats.PointsParseFail, 1)
				if s.Diagnostic != nil {
					s.Diagnostic.ParseError(err)
				}
				continue
			}

			for _, point := range points {
				s.batcher.In() <- point
			}
			atomic.AddInt64(&s.stats.PointsReceived, int64(len(points)))
		}
	}
}

// Close closes the service and the underlying listener.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed() {
		return nil // Already closed.
	}
	close(s.done)

	if s.conn != nil {
		s.conn.Close()
	}

	s.batcher.Flush()
	s.wg.Wait()

	// Release all remaining resources.
	s.done = nil
	s.conn = nil

	if s.Diagnostic != nil {
		s.Diagnostic.Closed()
	}

	return nil
}

// Closed returns true if the service is currently closed.
func (s *Service) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed()
}

func (s *Service) closed() bool {
	select {
	case <-s.done:
		// Service is closing.
		return true
	default:
	}
	return s.done == nil
}

// Ready returns true if the internal storage has been created successfully.
func (s *Service) Ready() bool {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	return ready
}

// Batcher returns the incoming point batcher for this service.
// This is primarily meant to be used for tests, but could also be used for manually
// inserting points into the udp database.
func (s *Service) Batcher() *tsdb.PointBatcher {
	return s.batcher
}

// createInternalStorage ensures that the required database has been created.
func (s *Service) createInternalStorage() error {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	if ready {
		return nil
	}

	if _, err := s.MetaClient.CreateDatabase(s.config.Database); err != nil {
		return err
	}

	// The service is now ready.
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) With(d Diagnostic) {
	s.Diagnostic = d
}

// Addr returns the listener's address.
func (s *Service) Addr() net.Addr {
	return s.addr
}
