package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/base"

	"github.com/aler9/rtsp-simple-server/internal/logger"
)

const (
	rtspSourceRetryPause = 5 * time.Second
)

type rtspSourceParent interface {
	Log(logger.Level, string, ...interface{})
	OnSourceExternalSetReady(req sourceExtSetReadyReq)
	OnSourceExternalSetNotReady(req sourceExtSetNotReadyReq)
	OnSourceFrame(int, gortsplib.StreamType, []byte)
}

type rtspSource struct {
	ur              string
	proto           *gortsplib.ClientProtocol
	anyPortEnable   bool
	fingerprint     string
	readTimeout     time.Duration
	writeTimeout    time.Duration
	readBufferCount int
	readBufferSize  int
	wg              *sync.WaitGroup
	stats           *stats
	parent          rtspSourceParent

	ctx       context.Context
	ctxCancel func()
}

func newRTSPSource(
	parentCtx context.Context,
	ur string,
	proto *gortsplib.ClientProtocol,
	anyPortEnable bool,
	fingerprint string,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	readBufferCount int,
	readBufferSize int,
	wg *sync.WaitGroup,
	stats *stats,
	parent rtspSourceParent) *rtspSource {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &rtspSource{
		ur:              ur,
		proto:           proto,
		anyPortEnable:   anyPortEnable,
		fingerprint:     fingerprint,
		readTimeout:     readTimeout,
		writeTimeout:    writeTimeout,
		readBufferCount: readBufferCount,
		readBufferSize:  readBufferSize,
		wg:              wg,
		stats:           stats,
		parent:          parent,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
	}

	atomic.AddInt64(s.stats.CountSourcesRTSP, +1)
	s.log(logger.Info, "started")

	s.wg.Add(1)
	go s.run()

	return s
}

func (s *rtspSource) Close() {
	atomic.AddInt64(s.stats.CountSourcesRTSP, -1)
	s.log(logger.Info, "stopped")
	s.ctxCancel()
}

// IsSource implements source.
func (s *rtspSource) IsSource() {}

// IsSourceExternal implements sourceExternal.
func (s *rtspSource) IsSourceExternal() {}

func (s *rtspSource) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[rtsp source] "+format, args...)
}

func (s *rtspSource) run() {
	defer s.wg.Done()

	for {
		ok := func() bool {
			ok := s.runInner()
			if !ok {
				return false
			}

			select {
			case <-time.After(rtspSourceRetryPause):
				return true
			case <-s.ctx.Done():
				return false
			}
		}()
		if !ok {
			break
		}
	}

	s.ctxCancel()
}

func (s *rtspSource) runInner() bool {
	s.log(logger.Debug, "connecting")

	client := &gortsplib.Client{
		Protocol: s.proto,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				h := sha256.New()
				h.Write(cs.PeerCertificates[0].Raw)
				hstr := hex.EncodeToString(h.Sum(nil))
				fingerprintLower := strings.ToLower(s.fingerprint)

				if hstr != fingerprintLower {
					return fmt.Errorf("server fingerprint do not match: expected %s, got %s",
						fingerprintLower, hstr)
				}

				return nil
			},
		},
		ReadTimeout:     s.readTimeout,
		WriteTimeout:    s.writeTimeout,
		ReadBufferCount: s.readBufferCount,
		ReadBufferSize:  s.readBufferSize,
		AnyPortEnable:   s.anyPortEnable,
		OnRequest: func(req *base.Request) {
			s.log(logger.Debug, "c->s %v", req)
		},
		OnResponse: func(res *base.Response) {
			s.log(logger.Debug, "s->c %v", res)
		},
	}

	innerCtx, innerCtxCancel := context.WithCancel(context.Background())

	var conn *gortsplib.ClientConn
	var err error
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		conn, err = client.DialReadContext(innerCtx, s.ur)
	}()

	select {
	case <-s.ctx.Done():
		innerCtxCancel()
		<-dialDone
		return false

	case <-dialDone:
		innerCtxCancel()
	}

	if err != nil {
		s.log(logger.Info, "ERR: %s", err)
		return true
	}

	s.log(logger.Info, "ready")

	s.parent.OnSourceExternalSetReady(sourceExtSetReadyReq{
		Tracks: conn.Tracks(),
	})

	defer func() {
		s.parent.OnSourceExternalSetNotReady(sourceExtSetNotReadyReq{})
	}()

	readErr := make(chan error)
	go func() {
		readErr <- conn.ReadFrames(func(trackID int, streamType gortsplib.StreamType, payload []byte) {
			s.parent.OnSourceFrame(trackID, streamType, payload)
		})
	}()

	select {
	case <-s.ctx.Done():
		conn.Close()
		<-readErr
		return false

	case err := <-readErr:
		s.log(logger.Info, "ERR: %s", err)
		conn.Close()
		return true
	}
}