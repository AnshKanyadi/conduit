package consensus

import (
	"context"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcdStore adapts a clientv3.Client to the Store interface.
// All etcd RPCs are forwarded directly; the interface boundary keeps the rest
// of the codebase free from etcd import paths.
type etcdStore struct {
	client *clientv3.Client
}

// NewEtcdStore dials etcd with cfg and returns a Store and the underlying
// client. The caller owns the client and must call client.Close() on shutdown.
//
// Why return the client separately?
// The Store interface doesn't expose Close. Returning the raw client lets
// main.go defer client.Close() without a type assertion.
func NewEtcdStore(cfg clientv3.Config) (Store, *clientv3.Client, error) {
	c, err := clientv3.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("consensus: etcd connect: %w", err)
	}
	return &etcdStore{client: c}, c, nil
}

func (e *etcdStore) Put(ctx context.Context, key, value string, leaseID int64) error {
	var opts []clientv3.OpOption
	if leaseID != 0 {
		opts = append(opts, clientv3.WithLease(clientv3.LeaseID(leaseID)))
	}
	_, err := e.client.Put(ctx, key, value, opts...)
	return err
}

func (e *etcdStore) Delete(ctx context.Context, key string) error {
	_, err := e.client.Delete(ctx, key)
	return err
}

func (e *etcdStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	resp, err := e.client.Get(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if len(resp.Kvs) == 0 {
		return nil, false, nil
	}
	return resp.Kvs[0].Value, true, nil
}

func (e *etcdStore) List(ctx context.Context, prefix string) ([]KV, error) {
	resp, err := e.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	kvs := make([]KV, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kvs = append(kvs, KV{Key: string(kv.Key), Value: kv.Value})
	}
	return kvs, nil
}

func (e *etcdStore) Watch(ctx context.Context, prefix string) <-chan WatchEvent {
	rch := e.client.Watch(ctx, prefix, clientv3.WithPrefix())
	out := make(chan WatchEvent, 64)

	go func() {
		defer close(out)
		for wresp := range rch {
			for _, ev := range wresp.Events {
				we := WatchEvent{
					KV: KV{
						Key:   string(ev.Kv.Key),
						Value: ev.Kv.Value,
					},
					Deleted: ev.Type == clientv3.EventTypeDelete,
				}
				select {
				case out <- we:
				default:
				}
			}
		}
	}()

	return out
}

func (e *etcdStore) Grant(ctx context.Context, ttl int64) (int64, error) {
	resp, err := e.client.Grant(ctx, ttl)
	if err != nil {
		return 0, err
	}
	return int64(resp.ID), nil
}

func (e *etcdStore) KeepAlive(ctx context.Context, leaseID int64) error {
	ch, err := e.client.KeepAlive(ctx, clientv3.LeaseID(leaseID))
	if err != nil {
		return err
	}
	// Drain keepalive responses; the channel closes when ctx is cancelled or
	// the lease expires.
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return fmt.Errorf("consensus: keepalive channel closed (lease expired?)")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *etcdStore) Revoke(ctx context.Context, leaseID int64) error {
	_, err := e.client.Revoke(ctx, clientv3.LeaseID(leaseID))
	return err
}
