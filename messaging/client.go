package messaging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// DefaultReconnectTimeout is the default time to wait between when a broker
// stream disconnects and another connection is retried.
const DefaultReconnectTimeout = 100 * time.Millisecond

// ClientConfig represents the Client configuration that must be persisted
// across restarts.
type ClientConfig struct {
	Brokers []*url.URL `json:"brokers"`
}

// NewClientConfig returns a new instance of ClientConfig.
func NewClientConfig(u []*url.URL) *ClientConfig {
	return &ClientConfig{
		Brokers: u,
	}
}

// Client represents a client for the broker's HTTP API.
type Client struct {
	mu     sync.Mutex
	conns  []*Conn
	config ClientConfig // The Client state that must be persisted to disk.

	opened bool
	done   chan chan struct{} // disconnection notification

	// The amount of time to wait before reconnecting to a broker stream.
	ReconnectTimeout time.Duration

	// The logging interface used by the client for out-of-band errors.
	Logger *log.Logger
}

// NewClient returns a new instance of Client with defaults set.
func NewClient() *Client {
	c := &Client{
		ReconnectTimeout: DefaultReconnectTimeout,
	}
	c.SetLogOutput(os.Stderr)
	return c
}

// URLs returns a list of broker URLs to connect to.
func (c *Client) URLs() []*url.URL {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.Brokers
}

// LeaderURL returns the URL of the broker leader.
func (c *Client) LeaderURL() *url.URL {
	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO(benbjohnson): Actually keep track of the leader.
	// HACK(benbjohnson): For testing, just grab a url.
	return c.config.Brokers[0]
}

// SetLogOutput sets writer for all Client log output.
func (c *Client) SetLogOutput(w io.Writer) {
	c.Logger = log.New(w, "[messaging] ", log.LstdFlags)
}

// Open initializes and opens the connection to the cluster. The
// URLs used to contact the cluster are either those supplied to
// the function, or if none are supplied, are read from the file
// at "path". These URLs do need to be URLs of actual Brokers.
// Regardless of URL source, at least 1 URL must be available
// for the client to be successfully opened.
func (c *Client) Open(path string, urls []*url.URL) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return error if the client is already open.
	if c.opened {
		return ErrClientOpen
	}

	// Read URLs from file if no URLs are provided.
	if len(urls) == 0 {
		// Read URLs from config file. There is no guarantee
		// that the Brokers URLs in the config file are still
		// the Brokers, so we're going to double-check.
		b, err := ioutil.ReadFile(path)
		if os.IsNotExist(err) {
			// nop
		} else if err != nil {
			return err
		} else {
			if err := json.Unmarshal(b, &c.config); err != nil {
				return err
			}
			urls = c.config.Brokers
		}
	}

	if len(urls) < 1 {
		return ErrBrokerURLRequired
	}

	// Now that we have the seed URLs, actually use these to
	// get the actual Broker URLs. Do that here.
	c.config.Brokers = urls // Let's pretend they are the same

	// Create a channel for streaming messages.
	c.c = make(chan *Message, 0)

	// Open the streamer if there's an ID set.
	if c.replicaID != 0 {
		c.done = make(chan chan struct{})
		go c.streamer(c.done)
	}

	// Set open flag.
	c.opened = true

	return nil
}

// Close disconnects the client from the broker cluster.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return error if the client is already closed.
	if !c.opened {
		return ErrClientClosed
	}

	// Shutdown streamer.
	if c.done != nil {
		ch := make(chan struct{})
		c.done <- ch
		<-ch
		c.done = nil
	}

	// Close message stream & clear index.
	close(c.c)
	c.c = nil
	c.index = 0

	// Unset open flag.
	c.opened = false

	return nil
}

// Publish sends a message to the broker and returns an index or error.
func (c *Client) Publish(m *Message) (uint64, error) {
	var resp *http.Response
	var err error

	u := *c.LeaderURL()
	for {
		// Send the message to the messages endpoint.
		u.Path = "/messaging/messages"
		u.RawQuery = url.Values{
			"type":    {strconv.FormatUint(uint64(m.Type), 10)},
			"topicID": {strconv.FormatUint(m.TopicID, 10)},
		}.Encode()
		resp, err = http.Post(u.String(), "application/octet-stream", bytes.NewReader(m.Data))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()

		// If a temporary redirect occurs then update the leader and retry.
		// If a non-200 status is returned then an error occurred.
		if resp.StatusCode == http.StatusTemporaryRedirect {
			redirectURL, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				return 0, fmt.Errorf("bad redirect: %s", resp.Header.Get("Location"))
			}
			u = *redirectURL
			continue
		} else if resp.StatusCode != http.StatusOK {
			if errstr := resp.Header.Get("X-Broker-Error"); errstr != "" {
				return 0, errors.New(errstr)
			}
			return 0, fmt.Errorf("cannot publish(%d)", resp.StatusCode)
		} else {
			break
		}
	}

	// Parse broker index.
	index, err := strconv.ParseUint(resp.Header.Get("X-Broker-Index"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid index: %s", err)
	}

	return index, nil
}

// Conn represents a stream over the client for a single topic.
type Conn struct {
	topicID uint64  // topic identifier
	index   uint64  // highest index sent over the channel
	url     url.URL // current broker url

	c         chan *Message // channel streams messages from the broker.
	reconnect chan struct{} // notification channel for broker change.
}

// NewConn returns a new connection to the broker for a topic.
func NewConn(topicID uint64, index uint64) *Conn {
	return &Conn{
		topicID: topicID,
		index:   index,
	}
}

// TopicID returns the connection's topic id.
func (c *Conn) TopicID() uint64 { return c.topicID }

// C returns streaming channel for the connection.
func (c *Conn) C() <-chan *Message { return c.c }

// Index returns the highest index sent over the channel.
func (c *Conn) Index() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index
}

// URL returns the current URL of the connection.
func (c *Conn) URL() url.URL {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// SetURL sets the current URL of the connection.
func (c *Conn) SetURL(u url.URL) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.url = u

	// Notify streamer of change.
	select {
	case c.reconnect <- struct{}{}:
	default:
	}
}

// Open opens a streaming connection to the broker.
func (c *Conn) Open() error {
}

// Heartbeat sends a heartbeat back to the broker with the client's index.
func (c *Conn) Heartbeat() error {
	var resp *http.Response
	var err error

	// Retrieve the parameters under lock.
	c.mu.Lock()
	topicID, index, u := c.topicID, c.index, c.url
	c.mu.Unlock()

	// Send the message to the messages endpoint.
	u.Path = "/messaging/heartbeat"
	u.RawQuery = url.Values{
		"topicID": {strconv.FormatUint(topicID, 10)},
		"index":   {strconv.FormatUint(index, 10)},
	}.Encode()
	resp, err = http.Post(u.String(), "application/octet-stream", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// If the server returns a redirect then it's not the leader.
	// If it returns a non-200 code then return the error.
	if resp.StatusCode == http.StatusTemporaryRedirect {
		return ErrNoLeader
	} else if resp.StatusCode != http.StatusOK {
		if errstr := resp.Header.Get("X-Broker-Error"); errstr != "" {
			return errors.New(errstr)
		}
		return fmt.Errorf("heartbeat error: %d", resp.StatusCode)
	}
	return nil
}

// streamer connects to a broker server and streams the replica's messages.
func (c *Conn) streamer(done chan chan struct{}) {
	for {
		// Check for the client disconnection.
		select {
		case ch := <-done:
			close(ch)
			return
		default:
		}

		// TODO: Validate that there is at least one broker URL.

		// Connect to broker and stream.
		u := c.URL()
		u.Path = "/messaging/messages"
		if err := c.streamFromURL(&u, done); err == errDone {
			return
		} else if err != nil {
			c.Logger.Print(err)
		}
	}
}

// streamFromURL connects to a broker server and streams the replica's messages.
func (c *Client) streamFromURL(u *url.URL, done chan chan struct{}) error {
	// Set the replica id on the URL and open the stream.
	u.RawQuery = url.Values{"replicaID": {strconv.FormatUint(c.replicaID, 10)}}.Encode()
	resp, err := http.Get(u.String())
	if err != nil {
		time.Sleep(c.ReconnectTimeout)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	// Ensure that we received a 200 OK from the server before streaming.
	if resp.StatusCode != http.StatusOK {
		time.Sleep(c.ReconnectTimeout)
		c.Logger.Printf("reconnecting to broker: %s (status=%d)", u, resp.StatusCode)
		return nil
	}

	c.Logger.Printf("connected to broker: %s", u)

	// Continuously decode messages from request body in a separate goroutine.
	errNotify := make(chan error, 0)
	go func() {
		dec := NewMessageDecoder(resp.Body)
		for {
			// Decode message from the stream.
			m := &Message{}
			if err := dec.Decode(m); err != nil {
				errNotify <- err
				return
			}

			// TODO: Write broker set updates, do not passthrough to channel.

			// Write message to streaming channel.
			c.c <- m

			// Update the index on the client.
			c.mu.Lock()
			if m.Index > c.index {
				c.index = m.Index
			}
			c.mu.Unlock()
		}
	}()

	// Check for the client disconnect or error from the stream.
	select {
	case ch := <-done:
		// Close body.
		_ = resp.Body.Close()

		// Clear message buffer.
		select {
		case <-c.c:
		default:
		}

		// Notify the close function and return marker error.
		close(ch)
		return errDone

	case err := <-errNotify:
		return err
	}
}

// marker error for the streamer.
var errDone = errors.New("done")
