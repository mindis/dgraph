/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/table"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/x"
)

var (
	ErrConnected    = errors.New("Edge already connected to another node.")
	ErrValue        = errors.New("Edge already has a value.")
	ErrEmptyXid     = errors.New("Empty XID node.")
	ErrInvalidType  = errors.New("Invalid value type")
	ErrEmptyVar     = errors.New("Empty variable name.")
	ErrNotConnected = errors.New("Edge needs to be connected to another node or a value.")
	emptyEdge       Edge
)

// BatchMutationOptions sets the clients batch mode to Pending number of buffers each of Size.
// Running counters of number of rdfs processed, total time and mutations per second are printed
// if PrintCounters is set true.  See Counter.
type BatchMutationOptions struct {
	Size          int
	Pending       int
	PrintCounters bool
}

var DefaultOptions = BatchMutationOptions{
	Size:          100,
	Pending:       100,
	PrintCounters: false,
}

type allocator struct {
	x.SafeMutex

	dc protos.DgraphClient

	kv  *badger.KV
	ids *cache

	syncCh chan entry

	startId uint64
	endId   uint64
}

// TODO: Add for n later
func (a *allocator) fetchOne() (uint64, error) {
	a.AssertLock()
	if a.startId == 0 || a.endId < a.startId {
		factor := time.Second
		for {
			assignedIds, err := a.dc.AssignUids(context.Background(), &protos.Num{Val: 1000})
			if err != nil {
				time.Sleep(factor)
				if factor < 256*time.Second {
					factor = factor * 2
				}
			} else {
				a.startId = assignedIds.StartId
				a.endId = assignedIds.EndId
				break
			}
		}
	}

	uid := a.startId
	a.startId++
	return uid, nil
}

func (a *allocator) getFromKV(id string) (uint64, error) {
	var item badger.KVItem
	if err := a.kv.Get([]byte(id), &item); err != nil {
		return 0, err
	}
	val := item.Value()
	if len(val) > 0 {
		uid, n := binary.Uvarint(val)
		if n <= 0 {
			return 0, fmt.Errorf("Unable to parse val %q to uint64 for %q", val, id)
		}
		return uid, nil
	}
	return 0, nil
}

func (a *allocator) assignOrGet(id string) (uid uint64, isNew bool, err error) {
	a.Lock()
	defer a.Unlock()
	uid, _ = a.ids.Get(id)
	if uid > 0 {
		return
	}

	// check in kv
	// get in outside lock and do put if missing ???
	uid, err = a.getFromKV(id)
	if err != nil {
		return
	}
	// if not found in kv
	if uid == 0 {
		// assign one
		uid, err = a.fetchOne()
		if err != nil {
			return
		}
		// Entry would be comitted to disc by the time it's evicted
		// TODO: Better to delete after it's persisted, can cause race
		// may be persist it during eviction and delete after it's synced
		// to disk
		a.syncCh <- entry{key: id, value: uid}
	}
	a.ids.Add(id, uid)
	isNew = true
	err = nil
	return
}

// Counter keeps a track of various parameters about a batch mutation. Running totals are printed
// if BatchMutationOptions PrintCounters is set to true.
type Counter struct {
	// Number of RDF's processed by server.
	Rdfs uint64
	// Number of mutations processed by the server.
	Mutations uint64
	// Time elapsed sinze the batch started.
	Elapsed time.Duration
}

// A Dgraph is the data structure held by the user program for all interactions with the Dgraph
// server.  After making grpc connection a new Dgraph is created by function NewDgraphClient.
type Dgraph struct {
	opts BatchMutationOptions

	schema chan protos.SchemaUpdate
	nquads chan nquadOp
	dc     []protos.DgraphClient
	wg     sync.WaitGroup
	alloc  *allocator
	ticker *time.Ticker

	// Miscellaneous information to print counters.
	// Num of RDF's sent
	rdfs uint64
	// Num of mutations sent
	mutations uint64
	// To get time elapsed.
	start time.Time
}

// NewDgraphClient creates a new Dgraph for interacting with the Dgraph store connected to in
// conns.  The Dgraph client stores blanknode to uid, and XIDnode to uid mappings on disk
// in clientDir.
//
// The client can be backed by multiple connections (to the same server, or multiple servers in a
// cluster).  
//
// A single client is thread safe for sharing with multiple go routines (though a single Req 
// should not be shared unless the go routines negotiate exclusive assess to the Req functions).  
func NewDgraphClient(conns []*grpc.ClientConn, opts BatchMutationOptions, clientDir string) *Dgraph {
	var clients []protos.DgraphClient
	for _, conn := range conns {
		client := protos.NewDgraphClient(conn)
		clients = append(clients, client)
	}
	return NewClient(clients, opts, clientDir)
}

// TODO(tzdybal) - hide this function from users
func NewClient(clients []protos.DgraphClient, opts BatchMutationOptions, clientDir string) *Dgraph {
	x.Check(os.MkdirAll(clientDir, 0700))
	opt := badger.DefaultOptions
	opt.SyncWrites = false
	opt.MapTablesTo = table.MemoryMap
	opt.Dir = clientDir
	opt.ValueDir = clientDir

	kv, err := badger.NewKV(&opt)
	x.Checkf(err, "Error while creating badger KV posting store")
	alloc := &allocator{
		dc:     clients[0],
		ids:    newCache(100000),
		kv:     kv,
		syncCh: make(chan entry, 10000),
	}

	d := &Dgraph{
		opts:   opts,
		dc:     clients,
		start:  time.Now(),
		nquads: make(chan nquadOp, opts.Pending*opts.Size),
		schema: make(chan protos.SchemaUpdate, opts.Pending*opts.Size),
		alloc:  alloc,
	}

	for i := 0; i < opts.Pending; i++ {
		d.wg.Add(1)
		go d.makeRequests()
	}
	d.wg.Add(1)
	go d.makeSchemaRequests()

	rand.Seed(time.Now().Unix())
	if opts.PrintCounters {
		go d.printCounters()
	}
	go d.batchSync()
	return d
}

// Close makes sure that the kv-store is closed properly. This should be called after using the
// Dgraph client.
func (d *Dgraph) Close() error {
	return d.alloc.kv.Close()
}

func (d *Dgraph) batchSync() {
	var entries []entry
	var loop uint64
	wb := make([]*badger.Entry, 0, 1000)

	for {
		ent := <-d.alloc.syncCh
	slurpLoop:
		for {
			entries = append(entries, ent)
			if len(entries) == 1000 {
				// Avoid making infinite batch, push back against syncCh.
				break
			}
			select {
			case ent = <-d.alloc.syncCh:
			default:
				break slurpLoop
			}
		}
		loop++
		//fmt.Printf("Writing batch of size: %v\n", len(entries))

		for _, e := range entries {
			// Atmost 10 bytes are needed for uvarint encoding
			buf := make([]byte, 10)
			n := binary.PutUvarint(buf[:], e.value)
			wb = badger.EntriesSet(wb, []byte(e.key), buf[:n])
		}
		if err := d.alloc.kv.BatchSet(wb); err != nil {
			fmt.Printf("Error while writing to disc %v\n", err)
		}
		for _, wbe := range wb {
			if err := wbe.Error; err != nil {
				fmt.Printf("Error while writing to disc %v\n", err)
			}
		}
		wb = wb[:0]
		entries = entries[:0]
	}
}

func (d *Dgraph) printCounters() {
	d.ticker = time.NewTicker(2 * time.Second)
	start := time.Now()

	for range d.ticker.C {
		counter := d.Counter()
		rate := float64(counter.Rdfs) / counter.Elapsed.Seconds()
		elapsed := ((time.Since(start) / time.Second) * time.Second).String()
		fmt.Printf("[Request: %6d] Total RDFs done: %8d RDFs per second: %7.0f Time Elapsed: %v \r",
			counter.Mutations, counter.Rdfs, rate, elapsed)

	}
}

func (d *Dgraph) request(req *Req) {
	counter := atomic.AddUint64(&d.mutations, 1)
	factor := time.Second
RETRY:
	_, err := d.dc[rand.Intn(len(d.dc))].Run(context.Background(), &req.gr)
	if err != nil {
		errString := err.Error()
		// Irrecoverable
		if strings.Contains(errString, "x509") || grpc.Code(err) == codes.Internal {
			log.Fatal(errString)
		}
		fmt.Printf("Retrying req: %d. Error: %v\n", counter, errString)
		time.Sleep(factor)
		if factor < 256*time.Second {
			factor = factor * 2
		}
		goto RETRY
	}
	req.reset()
}

func (d *Dgraph) makeRequests() {
	req := new(Req)

	for n := range d.nquads {
		if n.op == SET {
			req.Set(n.e)
		} else if n.op == DEL {
			req.Delete(n.e)
		}
		if req.size() == d.opts.Size {
			d.request(req)
		}
	}

	if req.size() > 0 {
		d.request(req)
	}
	d.wg.Done()
}

func (d *Dgraph) makeSchemaRequests() {
	req := new(Req)
LOOP:
	for {
		select {
		case s, ok := <-d.schema:
			if !ok {
				break LOOP
			}
			req.AddSchema(s)
		default:
			start := time.Now()
			if req.size() > 0 {
				d.request(req)
			}
			elapsedMillis := time.Since(start).Seconds() * 1e3
			if elapsedMillis < 10 {
				time.Sleep(time.Duration(int64(10-elapsedMillis)) * time.Millisecond)
			}
		}
	}

	if req.size() > 0 {
		d.request(req)
	}
	d.wg.Done()
}

// BatchSet adds Edge e as a set to the current batch mutation.  Once added, the client will apply
// the mutation to the Dgraph server when it is ready to flush its buffers.  The edge will be added
// to one of the batches as specified in d's BatchMutationOptions.  If that batch fills, it
// eventually flushes.  But there is no guarantee of delivery before BatchFlush() is called.
func (d *Dgraph) BatchSet(e Edge) error {
	d.nquads <- nquadOp{
		e:  e,
		op: SET,
	}
	atomic.AddUint64(&d.rdfs, 1)
	return nil
}

// BatchDelete adds Edge e as a delete to the current batch mutation.  Once added, the client will
// apply the mutation to the Dgraph server when it is ready to flush its buffers.  The edge will
// be added to one of the batches as specified in d's BatchMutationOptions.  If that batch fills,
// it eventually flushes.  But there is no guarantee of delivery before BatchFlush() is called.
func (d *Dgraph) BatchDelete(e Edge) error {
	d.nquads <- nquadOp{
		e:  e,
		op: DEL,
	}
	atomic.AddUint64(&d.rdfs, 1)
	return nil
}

// AddSchema adds the given schema mutation to the batch of schema mutations.  If the schema
// mutation applies an index to a UID edge, or if it adds reverse to a scalar edge, then the
// mutation is not added to the batch and an error is returned. Once added, the client will
// apply the schema mutation when it is ready to flush its buffers.
func (d *Dgraph) AddSchema(s protos.SchemaUpdate) error {
	if err := checkSchema(s); err != nil {
		return err
	}
	d.schema <- s
	return nil
}

// BatchFlush waits for all pending requests to complete. It should always be called after all
// BatchSet and BatchDeletes have been called.  Calling BatchFlush ends the client session and
// will cause a panic if further AddSchema, BatchSet or BatchDelete functions are called.
func (d *Dgraph) BatchFlush() {
	close(d.nquads)
	close(d.schema)
	d.wg.Wait()
	if d.ticker != nil {
		d.ticker.Stop()
	}
}

// Run runs the request in req and returns with the completed response from the server.  Calling
// Run has no effect on batched mutations.
//
// Mutations in the request are run before a query --- except when query variables link the
// mutation and query (see for example NodeUidVar) when the query is run first.
//
// Run returns a protos.Response which has the following fields
//
// - L : Latency information
//
// - Schema : Result of a schema query
//
// - AssignedUids : a map[string]uint64 of blank node name to assigned UID (if the query string
// contained a mutation with blank nodes)
//
// - N : Slice of *protos.Node returned by the query (Note: protos.Node not client.Node).
//
// There is an N[i], with Attribute "_root_", for each named query block in the query added to req.
// The N[i] also have a slice of nodes, N[i].Children each with Attribute equal to the query name, 
// for every answer to that query block.  From there, the Children represent nested blocks in the 
// query, the Attribute is the edge followed and the Properties are the scalar edges.
//
// Print a response with 
// 	"github.com/gogo/protobuf/proto"
// 	...
// 	req.SetQuery(`{
// 		friends(func: eq(name, "Alex")) {
//			name
//			friend {
// 				name
//			}
//		}	
//	}`)
// 	...
// 	resp, err := dgraphClient.Run(context.Background(), &req)
// 	fmt.Printf("%+v\n", proto.MarshalTextString(resp))
// Outputs
//	n: <
//	  attribute: "_root_"
//	  children: <
//	    attribute: "friends"
//	    properties: <
//	      prop: "name"
//	      value: <
//	        str_val: "Alex"
//	      >
//	    >
//	    children: <
//	      attribute: "friend"
//	      properties: <
//	        prop: "name"
//	        value: <
//	          str_val: "Chris"
//	        >
//	      >
//	    >
//	...
//
// It's often easier to unpack directly into a struct with Unmarshal, than to
// step through the response.
func (d *Dgraph) Run(ctx context.Context, req *Req) (*protos.Response, error) {
	return d.dc[rand.Intn(len(d.dc))].Run(ctx, &req.gr)
}

// Counter returns the current state of the BatchMutation.
func (d *Dgraph) Counter() Counter {
	return Counter{
		Rdfs:      atomic.LoadUint64(&d.rdfs),
		Mutations: atomic.LoadUint64(&d.mutations),
		Elapsed:   time.Since(d.start),
	}
}

// CheckVersion checks if the version of dgraph and dgraphloader are the same.  If either the
// versions don't match or the version information could not be obtained an error message is
// printed.
func (d *Dgraph) CheckVersion(ctx context.Context) {
	v, err := d.dc[rand.Intn(len(d.dc))].CheckVersion(ctx, &protos.Check{})
	if err != nil {
		fmt.Printf(`Could not fetch version information from Dgraph. Got err: %v.`, err)
	} else {
		version := x.Version()
		if version != "" && v.Tag != "" && version != v.Tag {
			fmt.Printf(`
Dgraph server: %v, loader: %v dont match.
You can get the latest version from https://docs.dgraph.io
`, v.Tag, version)
		}
	}
}

// NodeUid creates a Node from the given uint64.
func (d *Dgraph) NodeUid(uid uint64) Node {
	return Node{uid: uid}
}

// NodeBlank creates or returns a Node given a string name for the blank node. Blank nodes do not
// exist as labelled nodes in Dgraph. Blank nodes are used as labels client side for loading and
// linking nodes correctly.  If the label is new in this session a new UID is allocated and
// assigned to the label.  If the label has already been assigned, the corresponding Node
// is returned.
func (d *Dgraph) NodeBlank(varname string) (Node, error) {
	if len(varname) == 0 {
		d.alloc.Lock()
		defer d.alloc.Unlock()
		uid, err := d.alloc.fetchOne()
		if err != nil {
			return Node{}, err
		}
		return Node{uid: uid}, nil
	}
	uid, _, err := d.alloc.assignOrGet("_:" + varname)
	if err != nil {
		return Node{}, err
	}
	return Node{uid: uid}, nil
}

// NodeXid creates or returns a Node given a string name for an XID node. An XID node identifies a
// node with an edge _xid_, as in
// 	node --- _xid_ ---> XID string
// See https://docs.dgraph.io/query-language/#external-ids If the XID has already been allocated
// in this client session the allocated UID is returned, otherwise a new UID is allocated
// for xid and returned.
func (d *Dgraph) NodeXid(xid string, storeXid bool) (Node, error) {
	if len(xid) == 0 {
		return Node{}, ErrEmptyXid
	}
	uid, isNew, err := d.alloc.assignOrGet(xid)
	if err != nil {
		return Node{}, err
	}
	n := Node{uid: uid}
	if storeXid && isNew {
		e := n.Edge("xid")
		x.Check(e.SetValueString(xid))
		d.BatchSet(e)
	}
	return n, nil
}

// NodeUidVar creates a Node from a variable name.  When building a request, set and delete
// mutations may depend on the request's query, as in:
// https://docs.dgraph.io/query-language/#variables-in-mutations Such query variables in mutations
// could be built into the raw query string, but it is often more convenient to use client
// functions than manipulate strings.
//
// A request with a query and mutations (including variables in mutations) will run in the same
// manner as if the query and mutations were set into the query string.
func (d *Dgraph) NodeUidVar(name string) (Node, error) {
	if len(name) == 0 {
		return Node{}, ErrEmptyVar
	}

	return Node{varName: name}, nil
}
