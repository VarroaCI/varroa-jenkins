package recordstore

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// EnsureActivityStream applies design D2's ordered sequence for the
// varroa_activity transport buffer, on start and in every maintenance
// round:
//
//  1. Read the stream state. When the stream is absent, create it with
//     the fixed limits and stop.
//  2. Create or update the durable pull consumer "activity-recorder",
//     anchored to deliver every stored message.
//  3. Read the consumer state. When either the pending count or the
//     unacknowledged count is above zero, leave the stored limits
//     unchanged and stop.
//  4. When the stored configuration differs from the fixed limits,
//     update the stream once.
//
// Step 2 runs before step 4 on purpose: the consumer anchors at the
// first stored sequence, so a later limit change cannot remove a
// message the consumer has not seen. The recorder is the only caller;
// the BFF no longer creates or updates this stream.
func EnsureActivityStream(conn *bus.Conn, replicas int) error {
	target := bus.ActivityStreamConfig(bus.ActivityStreamName, replicas)

	if _, err := conn.StreamInfo(bus.ActivityStreamName); err != nil {
		if err := conn.EnsureStream(target); err != nil {
			return fmt.Errorf("create activity stream: %w", err)
		}
		return nil
	}

	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		return fmt.Errorf("ensure activity-recorder consumer: %w", err)
	}

	cinfo, err := conn.ConsumerInfo(bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		return fmt.Errorf("read activity-recorder consumer info: %w", err)
	}
	if cinfo.NumPending > 0 || cinfo.NumAckPending > 0 {
		// A shorter MaxAge applied now would delete a retained message
		// the consumer has not read yet.
		return nil
	}

	if err := conn.EnsureStream(target); err != nil {
		return fmt.Errorf("update activity stream: %w", err)
	}
	return nil
}
