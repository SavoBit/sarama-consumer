/*
  A simple kafka consumer-group client

  Copyright 2016 MistSys
*/

package consumer

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Shopify/sarama"
)

// minimum kafka API version required. Use this when constructing the sarama.Client's sarama.Config.MinVersion
var MinVersion = sarama.V0_9_0_0

// Error holds the errors generated by this package
type Error struct {
	Err     error
	Context string
	cl      *client
}

func (err Error) Error() string {
	return fmt.Sprintf("consumer-group %q: Error %s: %s", err.cl.group_name, err.Context, err.Err)
}

// Config is the configuration of a Client. Typically you'd create a default configuration with
// NewConfig, modofy any fields of interest, and pass it to NewClient. Once passed to NewClient the
// Config must not be modified. (doing so leads to data races, and may caused bugs as well)
type Config struct {
	Offsets struct {
		// The minimum interval between offset commits (defaults to 1s)
		Interval time.Duration
		Retry    struct {
			// The number of retries when comitting offsets (defaults to 3).
			Max int
		}
	}
	Session struct {
		// The allowed session timeout for registered consumers (defaults to 30s).
		// Must be within the allowed server range.
		Timeout time.Duration
	}
	Rebalance struct {
		// The allowed rebalance timeout for registered consumers (defaults to 30s).
		// Must be within the allowed server range. Only functions if sarama.Config.Version >= 0.10.1
		// Otherwise Session.Timeout is used for rebalancing too.
		Timeout time.Duration
	}
	Heartbeat struct {
		// Interval between each heartbeat (defaults to 3s). It should be no more
		// than 1/3rd of the Group.Session.Timout setting
		Interval time.Duration
	}
	// the partitioner used to map partitions to consumer group members (defaults to a round-robin partitioner)
	Partitioner Partitioner
}

// NewConfig constructs a default configuration.
func NewConfig() *Config {
	cfg := &Config{}
	cfg.Offsets.Interval = 1 * time.Second
	cfg.Offsets.Retry.Max = 3
	cfg.Session.Timeout = 30 * time.Second
	cfg.Rebalance.Timeout = 30 * time.Second
	cfg.Heartbeat.Interval = 3 * time.Second
	cfg.Partitioner = (*RoundRobin)(nil) // the infamous non-nil interface
	return cfg
}

/*
  NewClient creates a new consumer group client on top of an existing
  sarama.Client.

  After this call the contents of config should be treated as read-only.
  config can be nil if the defaults are acceptable.

  The consumer group name is used to match this client with other
  instances running elsewhere, but connected to the same cluster
  of kafka brokers and using the same consumer group name.
*/
func NewClient(group_name string, config *Config, sarama_client sarama.Client) (Client, error) {

	cl := &client{
		client:     sarama_client,
		config:     config,
		group_name: group_name,

		errors: make(chan error),

		closed:       make(chan struct{}),
		add_consumer: make(chan add_consumer),
		rem_consumer: make(chan *consumer),
	}

	// start the client's manager goroutine
	rc := make(chan error)
	go cl.run(rc)

	return cl, <-rc
}

/*
  Client is a kafaka client belonging to a consumer group.
*/
type Client interface {
	// Consume returns a consumer of the given topic
	Consume(topic string) (Consumer, error)

	// Close closes the client. It must be called to shutdown
	// the client after AsyncClose is complete in consumers.
	// It does NOT close the inner sarama.Client.
	Close()

	// Errors returns a channel which can (should) be monitored
	// for errors. callers should probably log or otherwise report
	// the returned errors. The channel closes when the client
	// is closed.
	Errors() <-chan error

	// TODO have a Status() method for debug/logging?
}

/*
  Consumer is a consumer of a topic.

  Messages from any partition assigned to this client arrive on the
  Messages channel, and errors arrive on the Errors channel. These operate
  the same as Messages and Errors in sarama.PartitionConsumer, except
  that messages and errors from any partition are mixed together.

  Every message read from the Messages channel must be eventually passed
  to Done. Calling Done is the signal that that message has been consumed
  and the offset of that message can be comitted back to kafka.

  Of course this requires that the message's Partition and Offset fields not
  be altered.
*/
type Consumer interface {
	// Messages returns the channel of messages arriving from kafka. It always
	// returns the same result, so it is safe to call once and store the result.
	// Every message read from the channel should be passed to Done when processing
	// of the message is complete.
	Messages() <-chan *sarama.ConsumerMessage

	// Done indicates the processing of the message is complete, and its offset can
	// be comitted to kafka. Calling Done twice with the same message, or with a
	// garbage message, can cause panics.
	Done(*sarama.ConsumerMessage)

	// Errors returns the channel of errors. These include errors from the underlying
	// partitions, as well as offset commit errors. Note that sarama's Config.Consumer.Return.Errors
	// is false by default, and without that most errors that occur within sarama are logged rather
	// than returned.
	Errors() <-chan error

	// AsyncClose terminates the consumer cleanly. Callers should continue to read from
	// Messages and Errors channels until they are closed. You must call AsyncClose before
	// closing the underlying sarama.Client.
	AsyncClose()
}

/*
  Partitioner maps partitions to consumer group members
*/
type Partitioner interface {
	// PrepareJoin prepares a JoinGroupRequest given the topics supplied.
	// The simplest implementation would be something like
	//   join_req.AddGroupProtocolMetadata("<partitioner name>", &sarama.ConsumerGroupMemberMetadata{ Version: 1, Topics:  topics, })
	PrepareJoin(join_req *sarama.JoinGroupRequest, topics []string)

	// Partition performs the partitioning. Given the requested
	// memberships from the JoinGroupResponse, it adds the results
	// to the SyncGroupRequest. Returning an error cancels everything.
	// The sarama.Client supplied to NewClient is included for convenince,
	// since performing the partitioning probably requires looking at the
	// topic's metadata, especially its list of partitions.
	Partition(*sarama.SyncGroupRequest, *sarama.JoinGroupResponse, sarama.Client) error

	// ParseSync parses the SyncGroupResponse and returns the map of topics
	// to partitions assigned to this client, or an error if the information
	// is not parsable.
	ParseSync(*sarama.SyncGroupResponse) (map[string][]int32, error)
}

// client implements the Client interface
type client struct {
	client     sarama.Client // the sarama client we were constructed from
	config     *Config       // our configuration (read-only)
	group_name string        // the client-group name

	errors chan error // channel over which asynchronous errors are reported

	closed       chan struct{}     // channel which is closed when the client is Close()ed
	add_consumer chan add_consumer // command channel used to add a new consumer
	rem_consumer chan *consumer    // command channel used to remove an existing consumer
}

func (cl *client) Errors() <-chan error { return cl.errors }

// add_consumer are the messages sent over the client.add_consumer channel
type add_consumer struct {
	con   *consumer
	reply chan<- error
}

func (cl *client) Consume(topic string) (Consumer, error) {
	con := &consumer{
		client:      cl,
		topic:       topic,
		messages:    make(chan *sarama.ConsumerMessage),
		errors:      make(chan error),
		assignments: make(chan assignment, 1),
	}

	reply := make(chan error)
	cl.add_consumer <- add_consumer{con, reply}
	err := <-reply
	if err != nil {
		return nil, err
	}
	return con, nil
}

func (cl *client) Close() {
	// signal to cl.run() that it should exit
	close(cl.closed)
}

// long lived goroutine which manages this client's membership in the consumer group
func (cl *client) run(early_rc chan<- error) {
	var member_id string                    // our group member id, assigned to us by kafka when we first make contact
	var generation_id int32                 // our current generation id
	consumers := make(map[string]*consumer) // map of topic -> consumer
	var wg sync.WaitGroup                   // waitgroup used to wait for all consumers to exit

	// add a consumer
	add := func(add add_consumer) {
		if _, ok := consumers[add.con.topic]; ok {
			// topic already is being consumed. the way the standard kafka 0.9 group coordination works you cannot consume twice with the
			// same client. If you want to consume the same topic twice, use two Clients.
			add.reply <- cl.makeError("Consume", fmt.Errorf("topic %q is already being consumed", add.con.topic))
		} else {
			consumers[add.con.topic] = add.con
			add.reply <- nil
		}
		wg.Add(1)
		go add.con.run(generation_id, &wg)
	}
	// remove a consumer
	rem := func(con *consumer) {
		existing_con := consumers[con.topic]
		if existing_con == con {
			delete(consumers, con.topic)
		} // else it's some old consumer and we've already removed it
	}
	// shutdown the consumers. waits until they are all stopped. only call once and return afterwards, since it makes assumptions that hold only when it is used like that
	shutdown := func() {
		// shutdown the remaining consumers
		for _, con := range consumers {
			con.AsyncClose()
		}
		// and consume any last rem_cosumer messages from them
		go func() {
			wg.Wait()
			close(cl.rem_consumer)
		}()
		for _ = range cl.rem_consumer {
			// toss the message; we're shutting down
		}
		// and shutdown the errors channel
		close(cl.errors)
	}

	pause := false
	for {

		// loop rejoining the group each time the group reforms
	join_loop:
		for {
			if pause {
				// pause before continuing, so we don't fail continuously too fast
				pause = false
				timeout := time.After(time.Second) // TODO should we increase timeouts?
			pause_loop:
				for {
					select {
					case <-timeout:
						break pause_loop
					case <-cl.closed:
						// shutdown the remaining consumers
						shutdown()
						return
					case a := <-cl.add_consumer:
						add(a)
					case r := <-cl.rem_consumer:
						rem(r)
					}
				}
			}

			// make contact with the kafka broker coordinating this group
			// NOTE: sarama keeps the result cached, so we aren't taking a round trip to the kafka brokers very time
			// (then again we need to manage sarama's cache too)
			coor, err := cl.client.Coordinator(cl.group_name)
			if err != nil {
				err = cl.makeError("contacting coordinating broker", err)
				if early_rc != nil {
					early_rc <- err
					return
				}
				cl.deliverError("", err)

				pause = true
				break join_loop
			}

			// join the group
			jreq := &sarama.JoinGroupRequest{
				GroupId:        cl.group_name,
				SessionTimeout: int32(cl.config.Session.Timeout / time.Millisecond),
				MemberId:       member_id,
				ProtocolType:   "consumer", // we implement the standard kafka 0.9 consumer protocol metadata
			}

			var topics = make([]string, 0, len(consumers))
			for topic := range consumers {
				topics = append(topics, topic)
			}
			cl.config.Partitioner.PrepareJoin(jreq, topics)

			jresp, err := coor.JoinGroup(jreq)
			if err != nil || jresp.Err == sarama.ErrNotCoordinatorForConsumer {
				// some I/O error happened, or the broker told us it is no longer the coordinator. in either case we should recompute the coordinator
				break join_loop
			}
			if err == nil && jresp.Err != 0 {
				err = jresp.Err
			}
			if err != nil {
				err = cl.makeError("joining group", err)
				// if it is still early (the 1st iteration of this loop) then return the error and bail out
				if early_rc != nil {
					early_rc <- err
					return
				}
				cl.deliverError("", err)

				pause = true
				continue join_loop
			}

			// we managed to get a successfull join-group response. that is far enough that basic communication is functioning
			// and we can declare that our early_rc is success and release the caller to NewClient
			if early_rc != nil {
				early_rc <- nil
				early_rc = nil
			}

			// save our member_id for next time we join, and the new generation id
			member_id = jresp.MemberId
			generation_id = jresp.GenerationId

			// prepare a sync request
			sreq := &sarama.SyncGroupRequest{
				GroupId:      cl.group_name,
				GenerationId: generation_id,
				MemberId:     jresp.MemberId,
			}

			// we have been chosen as the leader then we have to map the partitions
			if jresp.LeaderId == member_id {
				err := cl.config.Partitioner.Partition(sreq, jresp, cl.client)
				if err != nil {
					cl.deliverError("partitioning", err)
				}
			}

			// send SyncGroup
			sresp, err := coor.SyncGroup(sreq)
			if err != nil && sresp.Err == sarama.ErrNotCoordinatorForConsumer {
				//  we need a new coordinator
				break join_loop
			}
			if err == nil && sresp.Err != 0 {
				err = sresp.Err
			}
			if err != nil {
				cl.deliverError("synchronizing group", err)
				pause = true
				continue join_loop
			}
			assignments, err := cl.config.Partitioner.ParseSync(sresp)
			sresp.GetMemberAssignment()
			if err != nil {
				cl.deliverError("decoding member assignments", err)
				pause = true
				continue join_loop
			}

			// save and distribute the new assignments to our topic consumers
			a := assignment{
				generation_id: generation_id,
				assignments:   assignments,
			}
			for _, con := range consumers {
				select {
				case con.assignments <- a:
					// got it on the first try
				default:
					// con.assignment is full (it has a capacity of 1)
					// remove the stale assignment and place this one in its place
					select {
					case <-con.assignments:
						// we have room now (since we're the only code which writes to this channel)
						con.assignments <- a
					case con.assignments <- a:
						// in this case the consumer removed the stale assignment before we could
					}
				}
			}

			// start the heartbeat timer
			heartbeat_timer := time.After(cl.config.Heartbeat.Interval)

			// and loop, sending heartbeats until something happens and we need to rejoin (or exit)
		heartbeat_loop:
			for {
				select {
				case <-cl.closed:
					// cl.Close() has been called; time to exit
					resp, err := coor.LeaveGroup(&sarama.LeaveGroupRequest{
						GroupId:  cl.group_name,
						MemberId: jresp.MemberId,
					})
					if err == nil && resp.Err != 0 {
						err = resp.Err
					}
					if err != nil {
						cl.deliverError("leaving group", err)
					}

					// shutdown the remaining consumers
					shutdown()

					// and we're done
					return

				case <-heartbeat_timer:
					// send a heartbeat
					resp, err := coor.Heartbeat(&sarama.HeartbeatRequest{
						GroupId:      cl.group_name,
						MemberId:     member_id,
						GenerationId: generation_id,
					})
					if err != nil || resp.Err == sarama.ErrNotCoordinatorForConsumer {
						// we need a new coordinator
						break join_loop
					}
					if err != nil || resp.Err != 0 {
						// we've got heartbeat troubles of one kind or another; disconnect and reconnect
						break heartbeat_loop
					}

					// and start the next heartbeat only after we get the response to this one
					// that way when the network or the broker are slow we back off.
					heartbeat_timer = time.After(cl.config.Heartbeat.Interval)

				case a := <-cl.add_consumer:
					add(a)
					// and rejoin so we can become a member of the new topic
					continue join_loop
				case r := <-cl.rem_consumer:
					rem(r)
					// and rejoin so we can be removed as member of the new topic
					continue join_loop
				}
			} // end of heartbeat_loop
		} // end of join_loop

		// refresh the group coordinator (because sarama caches the result, and the cache must be manually invalidated by us when we decide it might be needed)
		err := cl.client.RefreshCoordinator(cl.group_name)
		if err != nil {
			err = cl.makeError("refreshing coordinating broker", err)
			if early_rc != nil {
				early_rc <- err
				return
			}
			cl.deliverError("", err)
			pause = true
		}
	}
}

// makeError builds an Error from an error from a lower level api (typically the sarama API)
func (cl *client) makeError(context string, err error) error {
	return Error{
		cl:      cl,
		Err:     err,
		Context: context,
	}
}

// deliverError builds an error and delivers it asynchronously to the channel returned by cl.Errors
func (cl *client) deliverError(context string, err error) {
	if context != "" {
		err = cl.makeError(context, err)
	}
	// deliver the error if anyone is listening. otherwise tough
	select {
	case cl.errors <- err:
	default:
	}
}

// consumer implements the Consumer interface
type consumer struct {
	client *client
	topic  string

	messages chan *sarama.ConsumerMessage
	errors   chan error

	wg         sync.WaitGroup // waitgroup signaling when all partition consumers have exited
	closed     chan struct{}  // channel which is closed when the consumed is Close()ed
	close_once sync.Once      // Once used to make sure we close only once

	assignments chan assignment // channel over which the client.run sends consumer.run each generation's partition assignments
}

// assignment is this client's assigned partitions
type assignment struct {
	generation_id int32
	assignments   map[string][]int32 // map of topic -> list of partitions
}

func (con *consumer) Messages() <-chan *sarama.ConsumerMessage { return con.messages }
func (con *consumer) Errors() <-chan error                     { return con.errors }

func (con *consumer) Done(*sarama.ConsumerMessage) {
}

// close the consumer. it can safely be called multiple times
func (con *consumer) AsyncClose() {
	con.close_once.Do(func() { close(con.closed) })
}

// consumer goroutine
func (con *consumer) run(generation_id int32, wg *sync.WaitGroup) {
	defer wg.Done()

	var partitions []int32 //  our current partition assignment

	partition_consumers := make(map[int32]chan int32) // map of partition -> channel used to command them

	for {
		select {
		case <-con.closed:
			// wait for the partition consumers to shutdown
			con.wg.Wait()
			// and close the output channels
			close(con.messages)
			close(con.errors)
			con.client.rem_consumer <- con
			// and we're done
			return

		case a := <-con.assignments:
			// see if we've gotten a different partition assignment than the current one
			new_partitions := a.assignments[con.topic]
			generation_id = a.generation_id
			added, removed := difference(partitions, new_partitions)
			for _, p := range removed {
				// stop consuming from partition p
				if command, ok := partition_consumers[p]; ok {
					delete(partition_consumers, p)
					command <- -1 // generation -1 is a magic value within the kafka offset API, so we reuse it here to indicate a stop
				}
			}
			for _, p := range added {
				// start consuming from partition p
				command := make(chan int32)
				partition_consumers[p] = command
				// TODO NSD HERE
			}
		}
	}
}

// difference returns the differences (additions and subtractions) between two slices of int32.
// typically the slices contain partition numbers.
func difference(a, b []int32) (added, removed []int32) {
	// can we assume a and b are sorted? The kafka spec doesn't say anything about partition lists being sorted, so best not to do so
	x, y := make(int32Slice, len(a)), make(int32Slice, len(b))
	copy(x, a)
	sort.Sort(x)
	copy(y, b)
	sort.Sort(y)

	i, j := 0, 0
	for i < len(x) && j < len(y) {
		if x[i] < y[j] {
			removed = append(removed, x[i])
			i++
		} else if x[i] > y[j] {
			added = append(added, y[j])
			j++
		} else {
			i++
			j++
		}
	}
	removed = append(removed, x[i:]...)
	added = append(added, y[j:]...)

	return
}

// a sortable []int32
type int32Slice []int32

func (p int32Slice) Len() int           { return len(p) }
func (p int32Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p int32Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// a simple partitioner that assigns partitions round-robin across all consumers requesting the topic
type RoundRobin struct{}

func (*RoundRobin) PrepareJoin(jreq *sarama.JoinGroupRequest, topics []string) {
	jreq.AddGroupProtocolMetadata("round-robin",
		&sarama.ConsumerGroupMemberMetadata{
			Version: 1,
			Topics:  topics,
		})
}

// for each topic in jresp, assign the topic's partitions round-robin across the members requesting the topic
func (*RoundRobin) Partition(sreq *sarama.SyncGroupRequest, jresp *sarama.JoinGroupResponse, client sarama.Client) error {
	by_member, err := jresp.GetMembers()
	if err != nil {
		return err
	}
	// invert the data, so we have the requests grouped by topic (they arrived grouped by member, since the kafka broker treats the data from each consumer as an opaque blob, so it couldn't do this step for us)
	by_topic := make(map[string][]string)
	for member, request := range by_member {
		if request.Version != 1 {
			// skip unsupported versions. we'll only assign to clients we can understand. Since we are such a client
			// we won't block all consumers (at least for those topics we consume). If this ends up a bad idea, we
			// can always change this code to return an error.
			continue
		}
		for _, topic := range request.Topics {
			by_topic[topic] = append(by_topic[topic], member)
		}
	}

	// finally, build our map the partitions of each topic
	assignments := make(map[string]map[string][]int32) // map of member to topics, and topic to partitions
	for topic, members := range by_topic {
		partitions, err := client.Partitions(topic)
		if err != nil {
			// what to do? we could maybe skip the topic, assigning it to no-one. But I/O errors are likely to happen again.
			// so let's stop partitioning and return the error.
			return err
		}
		if len(partitions) == 0 { // can this happen? best not to /0 later if it can
			// no one gets anything assigned. it is as if this topic didn't exist
			continue
		}

		for i, member_id := range members {
			topics, ok := assignments[member_id]
			if !ok {
				topics = make(map[string][]int32)
				assignments[member_id] = topics
			}
			partition := partitions[i%len(partitions)] // the round-robin bit (in case anyone lost track in all the mappings)
			topics[topic] = append(topics[topic], partition)
		}
	}

	// and encode the assignments in the sync request
	for member_id, topics := range assignments {
		sreq.AddGroupAssignmentMember(member_id,
			&sarama.ConsumerGroupMemberAssignment{
				Version: 1,
				Topics:  topics,
			})
	}

	return nil
}

func (*RoundRobin) ParseSync(sresp *sarama.SyncGroupResponse) (map[string][]int32, error) {
	ma, err := sresp.GetMemberAssignment()
	if err != nil {
		return nil, err
	}
	if ma.Version != 1 {
		return nil, fmt.Errorf("unsupported MemberAssignment version %d", ma.Version)
	}
	return ma.Topics, nil
}
