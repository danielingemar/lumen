package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Sender struct {
	URL    string
	APIKey string
	Client *http.Client
	Retry  time.Duration // base backoff, default 1s
}

// Post sends a gzip-compressed OTLP/JSON body. 5xx and network errors are retried
// (3 attempts); 4xx are returned immediately since retrying cannot fix them.
func (s *Sender) Post(ctx context.Context, path string, body []byte) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(body)
	gz.Close()
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if s.Retry == 0 {
		s.Retry = time.Second
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.Retry << (attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", s.URL+path, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		if s.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.APIKey)
		}
		resp, err := s.Client.Do(req)
		if err != nil {
			last = err
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode < 500:
			return fmt.Errorf("lumen rejected %s: %s", path, resp.Status)
		}
		last = fmt.Errorf("lumen %s: %s", path, resp.Status)
	}
	return last
}
