package kgo

import (
	"time"

	"github.com/twmb/kgo/kerr"
)

func (c *Client) triggerUpdateMetadata() {
	select {
	case c.updateMetadataCh <- struct{}{}:
	default:
	}
}

// updateMetadataLoop updates metadata whenever the update ticker ticks,
// or whenever deliberately triggered.
func (c *Client) updateMetadataLoop() {
	var consecutiveErrors int

	defer c.metadataTicker.Stop()
	for {
		select {
		case <-c.closedCh:
			return
		case <-c.metadataTicker.C:
			c.triggerUpdateMetadata()
		case <-c.updateMetadataCh:
			again, err := c.updateMetadata()
			if again || err != nil {
				c.triggerUpdateMetadata()
			}

			// If we did not error, we sleep 1s before the next
			// update to avoid unnecessary updates.
			// If we did error, we obey the backoff function.
			sleep := time.Second
			if err != nil {
				consecutiveErrors++
				sleep = c.cfg.client.retryBackoff(consecutiveErrors)
			} else {
				consecutiveErrors = 0
			}

			select {
			case <-c.closedCh:
				return
			case <-time.After(sleep):
			}
		}
	}
}

// updateMetadata updates all of a client's topic's metadata, returning whether
// a new update needs scheduling or if an error occured.
//
// If any topics or partitions have an error, all record buffers in the topic,
// or the record buffer for each erroring partition, has the first batch's
// try count bumped by one.
func (c *Client) updateMetadata() (needsRetry bool, err error) {
	topics := c.loadTopics()
	toUpdate := make([]string, 0, len(topics))
	for topic := range topics {
		toUpdate = append(toUpdate, topic)
	}

	meta, err := c.fetchTopicMetadata(toUpdate)
	if err != nil {
		return true, err
	}

	// Merge the producer side of the update.
	for topic, oldParts := range topics {
		newParts, exists := meta[topic]
		if !exists {
			continue
		}
		needsRetry = oldParts.merge(newParts) || needsRetry
	}

	// Trigger any consumer updates.
	c.consumer.doOnMetadataUpdate()

	return needsRetry, nil
}

// fetchTopicMetadata fetches metadata for all reqTopics and returns new
// topicPartitionsData for each topic.
func (c *Client) fetchTopicMetadata(reqTopics []string) (map[string]*topicPartitionsData, error) {
	meta, err := c.fetchMetadata(false, reqTopics)
	if err != nil {
		return nil, err
	}

	topics := make(map[string]*topicPartitionsData, len(reqTopics))

	c.brokersMu.RLock()
	defer c.brokersMu.RUnlock()

	for i := range meta.TopicMetadata {
		topicMeta := &meta.TopicMetadata[i]

		parts := &topicPartitionsData{
			loadErr:  kerr.ErrorForCode(topicMeta.ErrorCode),
			all:      make(map[int32]*topicPartition, len(topicMeta.PartitionMetadata)),
			writable: make(map[int32]*topicPartition, len(topicMeta.PartitionMetadata)),
		}
		topics[topicMeta.Topic] = parts

		if parts.loadErr != nil {
			continue
		}

		for i := range topicMeta.PartitionMetadata {
			partMeta := &topicMeta.PartitionMetadata[i]

			p := &topicPartition{
				topic:     topicMeta.Topic,
				partition: partMeta.Partition,
				loadErr:   kerr.ErrorForCode(partMeta.ErrorCode),

				leader:      partMeta.Leader,
				leaderEpoch: partMeta.LeaderEpoch,

				records: &recordBuffer{
					recordBuffersIdx: -1, // required, see below
					lastAckedOffset:  -1,
				},
				consumption: &consumption{
					allConsumptionsIdx: -1, // same, see below
					offset:             -1, // required to not consume until needed
				},

				replicas: partMeta.Replicas,
				isr:      partMeta.ISR,
				offline:  partMeta.OfflineReplicas,
			}

			broker, exists := c.brokers[p.leader]
			if !exists {
				if p.loadErr == nil {
					p.loadErr = &errUnknownBrokerForPartition{p.topic, p.partition, p.leader}
				}
			} else {
				p.records.sink = broker.recordSink
				p.records.topicPartition = p

				p.consumption.source = broker.recordSource
				p.consumption.topicPartition = p
			}

			parts.partitions = append(parts.partitions, p.partition)
			parts.all[p.partition] = p
			if p.loadErr == nil {
				parts.writable[p.partition] = p
			}
		}
	}

	return topics, nil
}

// merge merges a new topicPartition into an old and returns whether the
// metadata update that caused this merge needs to be retried.
//
// Retries are necessary if the topic or any partition has a retriable error.
func (l *topicPartitions) merge(r *topicPartitionsData) (needsRetry bool) {
	defer func() {
		// Lock&Unlock guarantees that anything that loaded the value
		// before our broadcast but had not reached Wait will hit
		// the wait before we broadcast.
		l.mu.Lock()
		l.mu.Unlock()

		l.c.Broadcast()
	}()

	lv := *l.load() // copy so our field writes do not collide with reads
	defer func() { l.v.Store(&lv) }()

	lv.loadErr = r.loadErr
	if r.loadErr != nil {
		retriable := kerr.IsRetriable(r.loadErr)
		if retriable {
			for _, topicPartition := range lv.all {
				topicPartition.records.bumpTriesAndMaybeFailBatch0(lv.loadErr)
			}
		} else {
			for _, topicPartition := range lv.all {
				topicPartition.records.failAllRecords(lv.loadErr)
			}
		}
		return retriable
	}

	lv.partitions = r.partitions

	var deleted []*topicPartition // should end up empty

	// Migrating topicPartitions is a little tricky because we have to
	// worry about map contents.
	//
	// We update everything appropriately in the new r.all, and after
	// this loop we copy the updated map to lv.all (which is stored
	// atomically after the defer above).
	for part, oldTP := range lv.all {
		newTP, exists := r.all[part]
		if !exists {
			deleted = append(deleted, oldTP)
			continue
		}

		if newTP.loadErr != nil { // partition errors should generally be temporary
			err := newTP.loadErr
			*newTP = *oldTP
			newTP.loadErr = err
			newTP.records.bumpTriesAndMaybeFailBatch0(newTP.loadErr)
			needsRetry = true
			continue
		}

		// If the new sink is the same as the old, we simply copy over
		// the records pointer and maybe begin draining again.
		//
		// We do not need to do anything to the consumption here.
		if newTP.records.sink == oldTP.records.sink {
			newTP.records = oldTP.records
			newTP.records.resetBackoffAndMaybeTriggerSinkDrain()
			continue
		}

		oldTP.migrateProductionTo(newTP)
		oldTP.migrateConsumptionTo(newTP)

	}

	// Anything left with a negative allPartsRecsIdx is a new topic
	// partition. We use this to add the new tp's records to its sink.
	// Same reasoning applies to the consumption offset.
	for _, newTP := range r.all {
		if newTP.records.recordBuffersIdx == -1 {
			newTP.records.sink.addSource(newTP.records)
		}
		if newTP.consumption.allConsumptionsIdx == -1 { // should be true if allPartsRecsIdx == -1
			newTP.consumption.source.addConsumption(newTP.consumption)
		}
	}

	lv.all = r.all
	lv.writable = r.writable
	// The left writable map needs no further updates: all changes above
	// happened to r.all, of which r.writable contains a subset of.
	// Modifications to r.all are seen in r.writable.

	if len(deleted) > 0 {
		go handleDeletedPartitions(deleted)
	}

	return needsRetry
}

// handleDeletedPartitions calls all promises in all records in all partitions
// in deleted with ErrPartitionDeleted.
//
// Kafka currently has no way to delete a partition, but, just in case.
func handleDeletedPartitions(deleted []*topicPartition) {
	for _, d := range deleted {
		d.records.mu.Lock()
		sink := d.records.sink
		sink.removeSource(d.records)
		for _, batch := range d.records.batches {
			for i, record := range batch.records {
				sink.broker.client.finishRecordPromise(record, ErrPartitionDeleted)
				batch.records[i] = noPNR
			}
			emptyRecordsPool.Put(&batch.records)
		}
		d.records.mu.Unlock()
	}
}