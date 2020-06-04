package kubernetes

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jpillora/backoff"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
)

type log struct {
	line   string
	offset int64
}

func (l log) String() string {
	if l.offset < 0 {
		return l.line
	}

	return fmt.Sprintf("%d %s", l.offset, l.line)
}

type brokenReaderError struct{}

func (e *brokenReaderError) Error() string {
	return "broken"
}

type brokenReader struct {
	err error
}

func newBrokenReader(err error) *brokenReader {
	return &brokenReader{err: err}
}

func (b *brokenReader) Read([]byte) (n int, err error) {
	return 0, b.err
}

func (b *brokenReader) Close() error {
	return nil
}

func TestNewKubernetesLogProcessor(t *testing.T) {
	client := new(kubernetes.Clientset)
	testBackoff := new(backoff.Backoff)
	logger := logrus.New()
	clientConfig := new(restclient.Config)
	p := newKubernetesLogProcessor(client, clientConfig, testBackoff, logger, kubernetesLogProcessorPodConfig{
		namespace: "namespace",
		pod:       "pod",
		container: "container",
		logPath:   "logPath",
	})

	assert.Equal(t, testBackoff, p.backoff)
	assert.Equal(t, logger, p.logger)
	require.NotNil(t, p.logStreamer)

	k, ok := p.logStreamer.(*kubernetesLogStreamer)
	assert.True(t, ok)
	assert.Equal(t, "namespace", k.namespace)
	assert.Equal(t, "pod", k.pod)
	assert.Equal(t, "container", k.container)
	assert.Equal(t, "namespace/pod/container/logPath", p.logStreamer.String())
}

func TestKubernetesLogStreamProviderLogStream(t *testing.T) {
	abortErr := errors.New("abort")

	namespace := "k8s_namespace"
	pod := "k8s_pod_name"
	container := "k8s_container_name"
	logPath := "log_path"

	client := mockKubernetesClientWithHost("", "", nil)
	cfg := new(restclient.Config)
	output := new(bytes.Buffer)
	offset := 15
	waitFileTimeout := time.Minute

	executor := new(MockRemoteExecutor)
	urlMatcher := mock.MatchedBy(func(url *url.URL) bool {
		query := url.Query()
		assert.Equal(t, container, query.Get("container"))
		assert.Equal(t, "true", query.Get("stdout"))
		assert.Equal(t, "true", query.Get("stderr"))
		command := query["command"]
		assert.Equal(t, []string{
			"gitlab-runner-helper",
			"read-logs",
			"--path",
			logPath,
			"--offset",
			strconv.Itoa(offset),
			"--wait-file-timeout",
			waitFileTimeout.String(),
		}, command)

		return true
	})
	executor.On("Execute", http.MethodPost, urlMatcher, cfg, nil, output, output, false).Return(abortErr)

	s := kubernetesLogStreamer{}
	s.client = client
	s.clientConfig = cfg
	s.executor = executor
	s.namespace = namespace
	s.pod = pod
	s.container = container
	s.logPath = logPath
	s.waitLogFileTimeout = waitFileTimeout

	err := s.Stream(context.Background(), int64(offset), output)
	assert.True(t, errors.Is(err, abortErr))
}

func TestReadLogsBrokenReader(t *testing.T) {
	proc := new(kubernetesLogProcessor)
	output := make(chan string)
	err := proc.readLogs(context.Background(), newBrokenReader(new(brokenReaderError)), output)

	assert.True(t, errors.Is(err, new(brokenReaderError)))
}

func TestProcessedOffsetSet(t *testing.T) {
	proc := new(kubernetesLogProcessor)

	ch := make(chan string)
	go func() {
		for range ch {
		}
	}()

	logs := logsToReader(
		log{line: "line 1", offset: 10},
		log{line: "line 1", offset: 20},
	)
	err := proc.readLogs(context.Background(), logs, ch)
	assert.NoError(t, err)
	assert.Equal(t, int64(20), proc.logsOffset)
}

func logsToReader(logs ...log) io.Reader {
	b := new(bytes.Buffer)
	for _, l := range logs {
		b.WriteString(l.String() + "\n")
	}

	return b
}

func TestParseLogs(t *testing.T) {
	tests := map[string]struct {
		line string

		expectedOffset int64
		expectedText   string
	}{
		"with offset": {
			line: "20 line",

			expectedOffset: 20,
			expectedText:   "line",
		},
		"with no offset": {
			line: "line",

			expectedOffset: -1,
			expectedText:   "line",
		},
		"starts with space": {
			line: " 20 line",

			expectedOffset: -1,
			expectedText:   " 20 line",
		},
		"multiple spaces after offset": {
			line: "20   line",

			expectedOffset: 20,
			expectedText:   "  line",
		},
		"empty log": {
			line: "",

			expectedOffset: -1,
			expectedText:   "",
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			p := new(kubernetesLogProcessor)

			offset, line := p.parseLogLine(tt.line)
			assert.Equal(t, tt.expectedOffset, offset)
			assert.Equal(t, tt.expectedText, line)
		})
	}
}

func TestListenReadLines(t *testing.T) {
	expectedLines := []string{"line 1", "line 2"}

	mockLogStreamer := newMockLogStreamer()
	defer mockLogStreamer.AssertExpectations(t)
	mockLogStreamer.On("Stream", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			writeLogs(
				args.Get(2).(io.Writer),
				log{line: expectedLines[0], offset: 10},
				log{line: expectedLines[1], offset: 20},
			)
		}).
		Return(nil).
		Once()

	processor := newTestKubernetesLogProcessor()
	processor.logStreamer = mockLogStreamer

	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan string)
	go processor.Process(ctx, ch)

	receivedLogs := make([]string, 0)
	for log := range ch {
		receivedLogs = append(receivedLogs, log)
		if len(receivedLogs) == len(expectedLines) {
			cancel()
		}
	}

	assert.Equal(t, expectedLines, receivedLogs)
}

func newMockLogStreamer() *mockLogStreamer {
	p := new(mockLogStreamer)
	p.On("String").Return("mockLogStreamer").Maybe()

	return p
}

func writeLogs(to io.Writer, logs ...log) {
	for _, l := range logs {
		_, _ = to.Write([]byte(l.String() + "\n"))
	}
}

func newTestKubernetesLogProcessor() *kubernetesLogProcessor {
	return &kubernetesLogProcessor{
		logger:  logrus.New(),
		backoff: newDefaultMockBackoffCalculator(),
	}
}

func newDefaultMockBackoffCalculator() *mockBackoffCalculator {
	c := new(mockBackoffCalculator)
	c.On("ForAttempt", mock.Anything).Return(time.Duration(0)).Maybe()

	return c
}

func TestListenCancelContext(t *testing.T) {
	mockLogStreamer := newMockLogStreamer()
	defer mockLogStreamer.AssertExpectations(t)

	ctx, _ := context.WithTimeout(context.Background(), 200*time.Millisecond)

	mockLogStreamer.On("Stream", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			<-ctx.Done()
		}).
		Return(io.EOF)

	processor := newTestKubernetesLogProcessor()
	processor.logStreamer = mockLogStreamer

	ch := make(chan string)
	go processor.Process(ctx, ch)
	for range ch {
	}
}

func TestAttachReconnectLogStream(t *testing.T) {
	const expectedConnectCount = 3
	ctx, cancel := context.WithCancel(context.Background())

	mockLogStreamer := newMockLogStreamer()
	defer mockLogStreamer.AssertExpectations(t)

	var connects int
	mockLogStreamer.
		On("Stream", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			connects++
			if connects == expectedConnectCount {
				cancel()
			}
		}).
		Return(io.EOF).
		Times(expectedConnectCount)

	mockBackoffCalculator := new(mockBackoffCalculator)
	defer mockBackoffCalculator.AssertExpectations(t)
	mockBackoffCalculator.On("ForAttempt", float64(1)).Return(time.Duration(0)).Once()
	mockBackoffCalculator.On("ForAttempt", float64(2)).Return(time.Duration(0)).Once()

	processor := new(kubernetesLogProcessor)
	processor.logger = logrus.New()
	processor.logStreamer = mockLogStreamer
	processor.backoff = mockBackoffCalculator

	ch := make(chan string)
	go processor.Process(ctx, ch)
	for range ch {
	}
}

func TestAttachReconnectReadLogs(t *testing.T) {
	const expectedConnectCount = 3
	ctx, cancel := context.WithCancel(context.Background())

	mockLogStreamer := newMockLogStreamer()
	defer mockLogStreamer.AssertExpectations(t)

	var connects int
	mockLogStreamer.
		On("Stream", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			_ = args.Get(2).(*io.PipeWriter).Close()

			connects++
			if connects == expectedConnectCount {
				cancel()
			}
		}).
		Return(nil).
		Times(expectedConnectCount)

	mockBackoffCalculator := new(mockBackoffCalculator)
	defer mockBackoffCalculator.AssertExpectations(t)
	mockBackoffCalculator.On("ForAttempt", float64(1)).Return(time.Duration(0)).Once()
	mockBackoffCalculator.On("ForAttempt", float64(2)).Return(time.Duration(0)).Once()

	processor := new(kubernetesLogProcessor)
	processor.logger = logrus.New()
	processor.logStreamer = mockLogStreamer
	processor.backoff = mockBackoffCalculator

	ch := make(chan string)
	go processor.Process(ctx, ch)
	for range ch {
	}
}

func TestAttachCorrectOffset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	mockLogStreamer := newMockLogStreamer()
	defer mockLogStreamer.AssertExpectations(t)

	mockLogStreamer.
		On("Stream", mock.Anything, int64(0), mock.Anything).
		Run(func(args mock.Arguments) {
			writeLogs(
				args.Get(2).(io.Writer),
				log{line: "line", offset: 10},
				log{line: "line", offset: 20},
			)
		}).
		Return(nil).
		Once()

	mockLogStreamer.
		On("Stream", mock.Anything, int64(20), mock.Anything).
		Run(func(mock.Arguments) {
			cancel()
		}).
		Return(new(brokenReaderError)).
		Once()

	processor := newTestKubernetesLogProcessor()
	processor.logStreamer = mockLogStreamer

	ch := make(chan string)
	go processor.Process(ctx, ch)
	for range ch {
	}
}

func TestScanHandlesStreamError(t *testing.T) {
	closedErr := errors.New("closed")
	processor := new(kubernetesLogProcessor)

	tests := map[string]struct {
		readerError   error
		expectedError error
	}{
		"reader EOF": {
			readerError: io.EOF,
			// EOF is handled specially. Since it means that the stream
			// reached its end, a nil is returned by scanner.Err()
			expectedError: nil,
		},
		"custom error": {
			readerError:   closedErr,
			expectedError: closedErr,
		},
	}

	for tn, tt := range tests {
		t.Run(tn, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			scanner, ch := processor.scan(ctx, newBrokenReader(tt.readerError))
			line, more := <-ch
			assert.Empty(t, line)
			assert.False(t, more)
			assert.True(t, errors.Is(scanner.Err(), tt.expectedError))
		})
	}
}

func TestScanHandlesCancelledContext(t *testing.T) {
	processor := new(kubernetesLogProcessor)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scanner, ch := processor.scan(ctx, logsToReader(log{}))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Block the channel, so there's no consumers
		time.Sleep(time.Second)

		// Assert that the channel is closed
		line, more := <-ch
		assert.Empty(t, line)
		assert.False(t, more)

		// Assert that the scanner had no error
		assert.Nil(t, scanner.Err())
	}()

	wg.Wait()
}
