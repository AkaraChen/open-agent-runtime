package vm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// qmpClient speaks just enough of the QEMU Machine Protocol to run human
// monitor commands (host port forwarding).
type qmpClient struct {
	path string
	mu   sync.Mutex
	c    net.Conn
	r    *bufio.Reader
}

func (q *qmpClient) connect() error {
	c, err := net.DialTimeout("unix", q.path, 2*time.Second)
	if err != nil {
		return err
	}
	q.c, q.r = c, bufio.NewReader(c)
	if _, err := q.r.ReadBytes('\n'); err != nil { // greeting
		c.Close()
		return err
	}
	if _, err := q.exec("qmp_capabilities", nil); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (q *qmpClient) exec(cmd string, args any) (json.RawMessage, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	q.c.SetDeadline(time.Now().Add(5 * time.Second))
	defer q.c.SetDeadline(time.Time{})
	if _, err := q.c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s", cmd, resp.Error.Desc)
		}
		if resp.Return != nil {
			return resp.Return, nil
		}
		// events are skipped
	}
}

// hmp runs a human monitor command; HMP reports failures as output text.
func (q *qmpClient) hmp(cmdline string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.c == nil {
		if err := q.connect(); err != nil {
			return err
		}
	}
	out, err := q.exec("human-monitor-command", map[string]string{"command-line": cmdline})
	if err != nil {
		q.c.Close()
		q.c = nil
		return err
	}
	var text string
	json.Unmarshal(out, &text)
	if text = strings.TrimSpace(text); text != "" {
		return fmt.Errorf("%s", text)
	}
	return nil
}
