// Copyright (2017) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

// miniplumber is a package to facilitate communication pipelines between
// registered readers and writers across a distributed meshage environment.
// miniplumber supports plumbing to external programs over stdio, similar to
// unix pipelines, supports trees of communication pipelines, and configurable
// delivery options.
package miniplumber

import (
	"bufio"
	"fmt"
	"math/rand"
	"meshage"
	log "minilog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	TIMEOUT = time.Duration(10 * time.Second)
)

const (
	SCHEDULE_ALL = -1
)

const (
	MODE_ALL = iota
	MODE_RR
	MODE_RND
)

const (
	MESSAGE_FORWARD = iota
	MESSAGE_QUERY
	MESSAGE_QUERY_RESPONSE
)

type Plumber struct {
	node      *meshage.Node         // meshage node to use for distributed environments
	Messages  chan *meshage.Message // incoming messages from meshage
	pipes     map[string]*Pipe
	pipelines map[string]*pipeline
	lock      sync.Mutex
	tidLock   sync.Mutex
	tids      map[int64]*tid
}

type tid struct {
	TID  int64
	C    chan *Message
	Done chan struct{}
	once sync.Once
}

type Pipe struct {
	name          string
	mode          int
	readers       map[int64]*Reader
	numWriters    int
	lock          sync.Mutex
	lastRecipient int64
}

type Reader struct {
	C    chan string
	Done chan struct{}
	once sync.Once
	ID   int64
}

type pipeline struct {
	name       string
	production []string
	done       chan bool
	canceler   sync.Once
}

type Message struct {
	TID       int64
	From      string
	Type      int
	Pipe      string
	Data      string
	Readers   []int64
	Recipient int64
}

type int64Sorter []int64

func (a int64Sorter) Len() int           { return len(a) }
func (a int64Sorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a int64Sorter) Less(i, j int) bool { return a[i] < a[j] }

func (t *tid) Close() {
	t.once.Do(func() {
		close(t.Done)
	})
}

func (r *Reader) Close() {
	r.once.Do(func() {
		close(r.Done)
	})
}

// New returns a new Plumber object over meshage node n
func New(n *meshage.Node) *Plumber {
	p := &Plumber{
		node:      n,
		Messages:  make(chan *meshage.Message, 1024),
		pipes:     make(map[string]*Pipe),
		pipelines: make(map[string]*pipeline),
		tids:      make(map[int64]*tid),
	}

	if p.node != nil {
		go p.handleMessages()
	}

	return p
}

func (p *Plumber) forward(pipe, data string, r int64) error {
	if p.node == nil {
		return nil
	}

	m := &Message{
		From:      p.node.Name(),
		Type:      MESSAGE_FORWARD,
		Pipe:      pipe,
		Data:      data,
		Recipient: r,
	}

	_, err := p.node.Broadcast(m)
	if err != nil {
		return err
	}

	return nil
}

func (p *Plumber) handleMessages() {
	for {
		m := (<-p.Messages).Body.(Message)

		log.Debug("got message type %v from %v", m.Type, m.From)

		switch m.Type {
		case MESSAGE_FORWARD:
			p.writeNoForward(m.Pipe, m.Data, m.Recipient)
		case MESSAGE_QUERY:
			p.sendReaders(&m)
		case MESSAGE_QUERY_RESPONSE:
			p.tidLock.Lock()
			t, ok := p.tids[m.TID]
			p.tidLock.Unlock()

			if !ok {
				log.Errorln("dropping message for invalid TID: ", m.TID)
				return
			}

			select {
			case t.C <- &m:
			case <-t.Done:
			}
		default:
			log.Error("unknown plumber message type: %v", m.Type)
		}
	}
}

func (p *Plumber) newTID() *tid {
	p.tidLock.Lock()
	defer p.tidLock.Unlock()

	s := rand.NewSource(time.Now().UnixNano())
	r := rand.New(s)

	t := &tid{
		TID:  r.Int63(),
		C:    make(chan *Message),
		Done: make(chan struct{}),
	}

	p.tids[t.TID] = t

	return t
}

func (p *Plumber) unregisterTID(t *tid) {
	t.Close()

	p.tidLock.Lock()
	p.tidLock.Unlock()

	if _, ok := p.tids[t.TID]; ok {
		delete(p.tids, t.TID)
	}
}

func (p *Plumber) sendReaders(m *Message) {
	p.lock.Lock()
	defer p.lock.Unlock()

	resp := &Message{
		TID:  m.TID,
		From: p.node.Name(),
		Type: MESSAGE_QUERY_RESPONSE,
		Pipe: m.Pipe,
	}

	if pp, ok := p.pipes[m.Pipe]; ok {
		pp.lock.Lock()
		defer pp.lock.Unlock()

		for k, _ := range pp.readers {
			m.Readers = append(m.Readers, k)
		}
	}

	_, err := p.node.Set([]string{m.From}, resp)
	if err != nil {
		log.Errorln(err)
	}
}

func (p *Plumber) Plumb(production ...string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// pipelines are unique by their string name, which may be an issue
	// down the road
	name := strings.Join(production, " ")

	log.Debug("got production: %v", name)

	if _, ok := p.pipelines[name]; ok {
		return fmt.Errorf("pipeline already exists: %v", production)
	}

	p.pipelines[name] = &pipeline{
		name:       name,
		production: production,
	}

	go p.startPipeline(p.pipelines[name])

	return nil
}

func (p *Plumber) Mode(pipe string, mode int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if _, ok := p.pipes[pipe]; !ok {
		p.pipes[pipe] = &Pipe{
			name:    pipe,
			readers: make(map[int64]*Reader),
		}
	}
	pp := p.pipes[pipe]

	pp.lock.Lock()
	defer pp.lock.Unlock()

	pp.mode = mode
}

func (p *Plumber) PipelineDelete(production ...string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	name := strings.Join(production, " ")
	if pp, ok := p.pipelines[name]; !ok {
		return fmt.Errorf("no such pipeline: %v", production)
	} else {
		pp.cancel()
		return nil
	}
}

func (p *Plumber) PipelineDeleteAll() error {
	p.lock.Lock()

	var keys []string
	for k, _ := range p.pipelines {
		keys = append(keys, k)
	}

	p.lock.Unlock()

	for _, k := range keys {
		err := p.PipelineDelete(k)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Plumber) PipeDelete(pipe string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if pp, ok := p.pipes[pipe]; ok {
		pp.lock.Lock()
		defer pp.lock.Unlock()

		if pp.numWriters != 0 {
			return fmt.Errorf("cannot delete named pipe with open writers")
		}

		// kill all of the readers
		for _, c := range pp.readers {
			c.Close()
		}
		delete(p.pipes, pipe)

		return nil
	} else {
		return fmt.Errorf("no such pipe: %v", pipe)
	}
}

func (p *Plumber) PipeDeleteAll() error {
	p.lock.Lock()

	var keys []string
	for k, _ := range p.pipes {
		keys = append(keys, k)
	}

	p.lock.Unlock()

	for _, k := range keys {
		err := p.PipeDelete(k)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Plumber) Pipes() []*Pipe {
	p.lock.Lock()
	p.lock.Unlock()

	var keys []string
	var ret []*Pipe

	for k, _ := range p.pipes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, v := range keys {
		ret = append(ret, p.pipes[v])
	}

	return ret
}

// Pipelines returns a sorted list of pipeline production strings
func (p *Plumber) Pipelines() []string {
	p.lock.Lock()
	p.lock.Unlock()

	var ret []string

	for k, _ := range p.pipelines {
		ret = append(ret, k)
	}

	sort.Strings(ret)

	return ret
}

func (p *Plumber) NewReader(pipe string) *Reader {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.newReader(pipe)
}

func (p *Plumber) id() int64 {
	s := rand.NewSource(time.Now().UnixNano())
	r := rand.New(s)
	return r.Int63()
}

// assume the lock is held
func (p *Plumber) newReader(pipe string) *Reader {
	log.Debug("newReader: %v", pipe)

	r := &Reader{
		C:    make(chan string),
		Done: make(chan struct{}),
		ID:   p.id(),
	}

	if _, ok := p.pipes[pipe]; !ok {
		p.pipes[pipe] = &Pipe{
			name:    pipe,
			readers: make(map[int64]*Reader),
		}
	}
	pp := p.pipes[pipe]
	pp.readers[r.ID] = r

	go func() {
		<-r.Done
		pp.lock.Lock()
		defer pp.lock.Unlock()
		close(r.C)
		delete(pp.readers, r.ID)
	}()

	return r
}

func (p *Plumber) NewWriter(pipe string) chan<- string {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.newWriter(pipe)
}

// assume the lock is held
func (p *Plumber) newWriter(pipe string) chan<- string {
	log.Debug("newWriter: %v", pipe)

	c := make(chan string)

	if _, ok := p.pipes[pipe]; !ok {
		p.pipes[pipe] = &Pipe{
			name: pipe,
		}
	}
	pp := p.pipes[pipe]
	pp.lock.Lock()
	pp.numWriters++
	pp.lock.Unlock()

	go func() {
		for v := range c {
			r := p.schedule(pipe)
			p.forward(pipe, v, r)
			pp.write(v, r)
		}
		pp.lock.Lock()
		pp.numWriters--
		pp.lock.Unlock()
	}()

	return c
}

func (p *Plumber) Write(pipe string, value string) {
	r := p.schedule(pipe)

	p.lock.Lock()
	defer p.lock.Unlock()

	p.forward(pipe, value, r)

	if pp, ok := p.pipes[pipe]; ok {
		pp.write(value, r)
	}
}

// Based on the pipe mode, choose a recipient - system wide. This means
// querying other plumber's state over meshage.
func (p *Plumber) schedule(pipe string) int64 {
	p.lock.Lock()
	defer p.lock.Unlock()

	var readers []int64

	pp, ok := p.pipes[pipe]
	if !ok {
		log.Error("pipe not found - something is wrong: %v", pipe)
		return SCHEDULE_ALL
	}

	pp.lock.Lock()
	defer pp.lock.Unlock()
	if pp.mode == MODE_ALL {
		return SCHEDULE_ALL
	}

	for k, _ := range pp.readers {
		readers = append(readers, k)
	}

	t := p.newTID()
	defer p.unregisterTID(t)

	// get readers from other plumbers
	m := &Message{
		TID:  t.TID,
		From: p.node.Name(),
		Type: MESSAGE_QUERY,
		Pipe: pipe,
	}

	nodes, err := p.node.Broadcast(m)
	if err != nil {
		log.Errorln(err)
		return SCHEDULE_ALL
	}

	// wait for n responses, or a timeout
	for i := 0; i < len(nodes); i++ {
		select {
		case resp := <-t.C:
			if log.WillLog(log.DEBUG) {
				log.Debugln("got response: ", resp)
			}
			readers = append(readers, resp.Readers...)
		case <-time.After(TIMEOUT):
			log.Errorln("timeout")
			return SCHEDULE_ALL
		}
	}

	sort.Sort(int64Sorter(readers))

	if len(readers) == 0 {
		return SCHEDULE_ALL
	}

	// pick a winner!
	switch pp.mode {
	case MODE_RR:
		i := sort.Search(len(readers), func(i int) bool { return readers[i] > pp.lastRecipient })
		if i == len(readers) {
			i = 0
		}
		pp.lastRecipient = readers[i]
		return readers[i]
	case MODE_RND:
		return readers[rand.Intn(len(readers))]
	}

	// we should never get here
	return SCHEDULE_ALL
}

// write to a named pipe without forwarding the message over meshage
func (p *Plumber) writeNoForward(pipe string, value string, r int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if pp, ok := p.pipes[pipe]; ok {
		pp.write(value, r)
	}
}

// started in a goroutine, don't assume the lock is held
func (p *Plumber) startPipeline(pl *pipeline) {
	pl.done = make(chan bool)

	go func() {
		<-pl.done
		p.lock.Lock()
		delete(p.pipelines, pl.name)
		p.lock.Unlock()
	}()

	var b <-chan string
	for i, e := range pl.production {
		log.Debug("starting pipeline production element: %v", e)

		// start a process if it looks like a process, otherwise create
		// a pipe
		f := fieldsQuoteEscape("\"", e)
		process, err := exec.LookPath(f[0])
		if err == nil {
			f[0] = process

			// don't write data on stdout/err if this is the last stage
			var write bool
			if i != len(pl.production)-1 {
				write = true
			}

			b, err = pl.exec(f, b, write)
			if err != nil {
				pl.cancel()
				log.Errorln(err)
				break
			}
			continue
		}

		// looks like a named pipe
		var in *Reader

		// don't produce output if this is the final stage
		if i != len(pl.production)-1 {
			in = p.NewReader(e)
		}

		var out chan<- string

		// don't produce input if this is the first stage
		if i != 0 {
			out = p.NewWriter(e)
		}
		b = pl.pipe(in, out, b)
	}
}

func (pl *pipeline) exec(production []string, in <-chan string, write bool) (<-chan string, error) {
	log.Debug("exec: %v, %v", production, write)

	var out chan string

	cmd := &exec.Cmd{
		Path: production[0],
		Args: production,
	}

	if in != nil {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}

		go func() {
			defer pl.cancel()

			for {
				select {
				case v := <-in:
					_, err := stdin.Write([]byte(v))
					if err != nil {
						log.Errorln(err)
						return
					}
				case <-pl.done:
					return
				}
			}
		}()
	}

	if write {
		out = make(chan string)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}

		go func() {
			defer pl.cancel()

			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				select {
				case out <- scanner.Text() + "\n":
				case <-pl.done:
					return
				}
				log.Debug("exec got: %v", scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				log.Errorln(err)
				return
			}
		}()
	}

	err := cmd.Start()
	if err != nil {
		return nil, err
	}

	// command is running

	go func() {
		<-pl.done
		cmd.Process.Kill()
	}()

	return out, nil
}
func (pl *pipeline) pipe(pin *Reader, pout chan<- string, in <-chan string) <-chan string {
	log.Debug("pipe")

	if in != nil {
		go func() {
			defer close(pout)
			defer pl.cancel()

			for {
				select {
				case v := <-in:
					select {
					case pout <- v:
					case <-pl.done:
						return
					}
					log.Debug("pipe got: %v", v)
				case <-pl.done:
					return
				}
			}
		}()
	}

	if pin != nil {
		go func() {
			defer pin.Close()
			<-pl.done
		}()
		return pin.C
	}

	return nil
}

func (pl *pipeline) cancel() {
	pl.canceler.Do(func() {
		log.Debug("closing pipeline: %v", pl.name)
		close(pl.done)
	})
}

func (p *Pipe) Name() string {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.name
}

func (p *Pipe) Mode() string {
	p.lock.Lock()
	defer p.lock.Unlock()

	switch p.mode {
	case MODE_ALL:
		return "all"
	case MODE_RR:
		return "round-robin"
	case MODE_RND:
		return "random"
	default:
		log.Fatal("unknown mode: %v", p.mode)
	}
	return ""
}

func (p *Pipe) NumReaders() int {
	p.lock.Lock()
	defer p.lock.Unlock()

	return len(p.readers)
}

func (p *Pipe) NumWriters() int {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.numWriters
}

// don't assume the plumber lock is held
func (p *Pipe) write(value string, r int64) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// messages must end in a newline, because things like scanners depend
	// on them. Add a newline if it doesn't already exist.
	if !strings.HasSuffix(value, "\n") {
		value += "\n"
	}

	if r == SCHEDULE_ALL {
		for _, c := range p.readers {
			log.Debug("write: %v", value)
			select {
			case <-c.Done:
				continue
			case c.C <- value:
			}
		}
	} else {
		if c, ok := p.readers[r]; ok {
			log.Debug("write: %v", value)
			select {
			case <-c.Done:
			case c.C <- value:
			}
		}
	}
}

// Return a slice of strings, split on whitespace, not unlike strings.Fields(),
// except that quoted fields are grouped.
// 	Example: a b "c d"
// 	will return: ["a", "b", "c d"]
func fieldsQuoteEscape(c string, input string) []string {
	log.Debug("fieldsQuoteEscape splitting on %v: %v", c, input)
	f := strings.Fields(input)
	var ret []string
	trace := false
	temp := ""

	for _, v := range f {
		if trace {
			if strings.Contains(v, c) {
				trace = false
				temp += " " + trimQuote(c, v)
				ret = append(ret, temp)
			} else {
				temp += " " + v
			}
		} else if strings.Contains(v, c) {
			temp = trimQuote(c, v)
			if strings.HasSuffix(v, c) {
				// special case, single word like 'foo'
				ret = append(ret, temp)
			} else {
				trace = true
			}
		} else {
			ret = append(ret, v)
		}
	}
	log.Debug("generated: %#v", ret)
	return ret
}

func trimQuote(c string, input string) string {
	if c == "" {
		log.Errorln("cannot trim empty space")
		return ""
	}
	var ret string
	for _, v := range input {
		if v != rune(c[0]) {
			ret += string(v)
		}
	}
	return ret
}
