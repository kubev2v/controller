package web

import (
	"bytes"
	"encoding/json"
	"github.com/gorilla/websocket"
	liberr "github.com/konveyor/controller/pkg/error"
	libmodel "github.com/konveyor/controller/pkg/inventory/model"
	"io/ioutil"
	"net/http"
	liburl "net/url"
	"reflect"
	"time"
)

//
// Header.
const (
	WatchHeader = "X-Watch"
)

//
// Event handler
type EventHandler interface {
	// The watch has started.
	Started()
	// Parity marker.
	// The watch has delivered the initial set
	// of `Created` events.
	Parity()
	// Resource created.
	Created(r Event)
	// Resource updated.
	Updated(r Event)
	// Resource deleted.
	Deleted(r Event)
	// An error has occurred.
	// The handler may call the Repair() on
	// the watch to repair the watch as desired.
	Error(*Watch, error)
	// The watch has ended.
	End()
}

//
// Stock event handler.
// Provides default event methods.
type StockEventHandler struct{}

//
// Watch has started.
func (r *StockEventHandler) Started() {}

//
// Watch has parity.
func (r *StockEventHandler) Parity() {}

//
// A model has been created.
func (r *StockEventHandler) Created(Event) {}

//
// A model has been updated.
func (r *StockEventHandler) Updated(Event) {}

//
// A model has been deleted.
func (r *StockEventHandler) Deleted(Event) {}

//
// An error has occurred reading events.
func (r *StockEventHandler) Error(*Watch, error) {}

//
// An event watch has ended.
func (r *StockEventHandler) End() {}

//
// REST client.
type Client struct {
	// Transport.
	Transport http.RoundTripper
	// Headers.
	Header http.Header
}

//
// HTTP GET (method).
func (r *Client) Get(url string, out interface{}) (status int, err error) {
	parsedURL, err := liburl.Parse(url)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	request := &http.Request{
		Header: r.Header,
		Method: http.MethodGet,
		URL:    parsedURL,
	}
	client := http.Client{Transport: r.Transport}
	response, err := client.Do(request)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	status = response.StatusCode
	content := []byte{}
	if status == http.StatusOK {
		defer func() {
			_ = response.Body.Close()
		}()
		content, err = ioutil.ReadAll(response.Body)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
		err = json.Unmarshal(content, out)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
	}

	return
}

//
// HTTP POST (method).
func (r *Client) Post(url string, in interface{}, out interface{}) (status int, err error) {
	parsedURL, err := liburl.Parse(url)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	body, _ := json.Marshal(in)
	reader := bytes.NewReader(body)
	request := &http.Request{
		Header: r.Header,
		Method: http.MethodPost,
		Body:   ioutil.NopCloser(reader),
		URL:    parsedURL,
	}
	client := http.Client{Transport: r.Transport}
	response, err := client.Do(request)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	status = response.StatusCode
	content := []byte{}
	if status == http.StatusOK {
		defer func() {
			_ = response.Body.Close()
		}()
		if out == nil {
			return
		}
		content, err = ioutil.ReadAll(response.Body)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
		err = json.Unmarshal(content, out)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
	}

	return
}

//
// Watch a resource.
func (r *Client) Watch(
	url string,
	resource interface{},
	handler EventHandler) (status int, w *Watch, err error) {
	//
	url = r.patchURL(url)
	dialer := websocket.DefaultDialer
	header := http.Header{
		WatchHeader: []string{"1"},
	}
	for k, v := range r.Header {
		header[k] = v
	}
	post := func(w *WatchReader) (pStatus int, pErr error) {
		socket, response, pErr := dialer.Dial(url, header)
		if pErr != nil {
			pErr = liberr.Wrap(pErr)
			return
		}
		pStatus = response.StatusCode
		switch pStatus {
		case http.StatusOK,
			http.StatusSwitchingProtocols:
			pStatus = http.StatusOK
			w.webSocket = socket
		}
		return
	}
	reader := &WatchReader{
		resource: resource,
		handler:  handler,
		repair:   post,
	}
	status, err = post(reader)
	if err != nil || status != http.StatusOK {
		return
	}

	w = &Watch{reader: reader}
	reader.start()

	return
}

//
// Patch the URL.
func (r *Client) patchURL(in string) (out string) {
	out = in
	url, err := liburl.Parse(in)
	if err != nil {
		return
	}
	switch url.Scheme {
	case "http":
		url.Scheme = "ws"
	case "https":
		url.Scheme = "wss"
	default:
		return
	}

	out = url.String()

	return
}

//
// Watch (event) reader.
type WatchReader struct {
	// Repair function.
	repair func(*WatchReader) (int, error)
	// Web socket.
	webSocket *websocket.Conn
	// Web resource.
	resource interface{}
	// Event handler.
	handler EventHandler
	// Started.
	started bool
	// Done.
	done bool
}

//
// Terminate.
func (r *WatchReader) Terminate() {
	r.done = true
	_ = r.webSocket.Close()
}

//
// Repair.
func (r *WatchReader) Repair() (status int, err error) {
	return r.repair(r)
}

//
// Dispatch events.
func (r *WatchReader) start() {
	if r.started {
		return
	}
	r.started = true
	r.done = false
	go func() {
		defer func() {
			_ = r.webSocket.Close()
			r.started = false
			r.done = true
			r.handler.End()
		}()
		r.handler.Started()
		for {
			event := Event{
				Resource: r.clone(r.resource),
				Updated:  r.clone(r.resource),
			}
			err := r.webSocket.ReadJSON(&event)
			if err != nil {
				if r.done {
					break
				}
				time.Sleep(time.Second * 3)
				r.handler.Error(&Watch{reader: r}, err)
			}
			switch event.Action {
			case libmodel.Parity:
				r.handler.Parity()
			case libmodel.Error:
				r.handler.Error(&Watch{reader: r}, nil)
			case libmodel.End:
				return
			case libmodel.Created:
				r.handler.Created(event)
			case libmodel.Updated:
				r.handler.Updated(event)
			case libmodel.Deleted:
				r.handler.Deleted(event)
			}
		}
	}()
}

//
// Clone resource.
func (r *WatchReader) clone(in interface{}) (out interface{}) {
	mt := reflect.TypeOf(in)
	mv := reflect.ValueOf(in)
	switch mt.Kind() {
	case reflect.Ptr:
		mt = mt.Elem()
		mv = mv.Elem()
	}
	object := reflect.New(mt).Elem()
	object.Set(mv)
	return object.Addr().Interface()
}

//
// Represents a watch.
type Watch struct {
	reader *WatchReader
}

//
// End the watch.
func (r *Watch) Repair() (err error) {
	status, err := r.reader.Repair()
	if err != nil {
		return
	}
	if status != http.StatusOK {
		err = liberr.New(http.StatusText(status))
	}

	return
}

//
// End the watch.
func (r *Watch) End() {
	r.reader.Terminate()
}

//
// The watch has not ended.
func (r *Watch) Alive() bool {
	return !r.reader.done
}
