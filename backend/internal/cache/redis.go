package cache

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	addr     string
	password string
	db       int
	timeout  time.Duration
}

func New(addr, password string, db int) *Client {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	return &Client{
		addr:     addr,
		password: password,
		db:       db,
		timeout:  3 * time.Second,
	}
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.exec(ctx, "PING")
	return err
}

func (c *Client) GetJSON(ctx context.Context, key string, dest any) (bool, error) {
	value, err := c.exec(ctx, "GET", key)
	if err != nil {
		if err == errRedisNil {
			return false, nil
		}
		return false, err
	}
	raw, ok := value.([]byte)
	if !ok {
		return false, fmt.Errorf("redis get %q returned %T", key, value)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, fmt.Errorf("decode redis json %q: %w", key, err)
	}
	return true, nil
}

func (c *Client) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode redis json %q: %w", key, err)
	}
	ttlSeconds := strconv.FormatInt(int64(ttl/time.Second), 10)
	_, err = c.exec(ctx, "SET", key, string(payload), "EX", ttlSeconds)
	return err
}

func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := c.exec(ctx, "GET", key)
	if err != nil {
		if err == errRedisNil {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, ok := value.([]byte)
	if !ok {
		return nil, false, fmt.Errorf("redis get %q returned %T", key, value)
	}
	return raw, true, nil
}

func (c *Client) SetBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	ttlSeconds := strconv.FormatInt(int64(ttl/time.Second), 10)
	_, err := c.exec(ctx, "SET", key, string(value), "EX", ttlSeconds)
	return err
}

func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.exec(ctx, "DEL", key)
	return err
}

var errRedisNil = fmt.Errorf("redis nil")

func (c *Client) exec(ctx context.Context, args ...string) (any, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := writeRESPCommand(conn, args...); err != nil {
		return nil, fmt.Errorf("write redis command: %w", err)
	}
	reader := bufio.NewReader(conn)
	value, err := readRESP(reader)
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("dial redis: %w", err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(c.timeout))
	}

	if c.password != "" {
		if err := writeRESPCommand(conn, "AUTH", c.password); err != nil {
			conn.Close()
			return nil, fmt.Errorf("redis auth write: %w", err)
		}
		if _, err := readRESP(bufio.NewReader(conn)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("redis auth: %w", err)
		}
	}

	if c.db > 0 {
		if err := writeRESPCommand(conn, "SELECT", strconv.Itoa(c.db)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("redis select write: %w", err)
		}
		if _, err := readRESP(bufio.NewReader(conn)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("redis select: %w", err)
		}
	}

	return conn, nil
}

func writeRESPCommand(conn net.Conn, args ...string) error {
	if _, err := fmt.Fprintf(conn, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return nil
}

func readRESP(reader *bufio.Reader) (any, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("read redis prefix: %w", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read redis line: %w", err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

	switch prefix {
	case '+':
		return line, nil
	case '-':
		if line == "nil" {
			return nil, errRedisNil
		}
		return nil, fmt.Errorf("redis error: %s", line)
	case ':':
		value, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse redis integer: %w", err)
		}
		return value, nil
	case '$':
		size, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parse redis bulk length: %w", err)
		}
		if size == -1 {
			return nil, errRedisNil
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, fmt.Errorf("read redis bulk payload: %w", err)
		}
		return payload[:size], nil
	default:
		return nil, fmt.Errorf("unsupported redis response prefix %q", prefix)
	}
}
