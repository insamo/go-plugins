package zookeeper

import (
	"errors"
	"path"
	"sync"

	"github.com/micro/go-micro/registry"
	"github.com/samuel/go-zookeeper/zk"
)

type zookeeperWatcher struct {
	client  *zk.Conn
	once    sync.Once
	stop    chan bool
	results chan result
}

type watchResponse struct {
	event   zk.Event
	service *registry.Service
	err     error
}

type result struct {
	res *registry.Result
	err error
}

func newZookeeperWatcher(r *zookeeperRegistry) (registry.Watcher, error) {
	var once sync.Once
	stop := make(chan bool, 1)

	zw := &zookeeperWatcher{
		client:  r.client,
		once:    once,
		stop:    stop,
		results: make(chan result),
	}
	go zw.watch()

	return zw, nil
}

func (zw *zookeeperWatcher) watchDir(key string, respChan chan watchResponse) {
	for {
		children, _, childEventCh, err := zw.client.ChildrenW(key)
		if err != nil {
			respChan <- watchResponse{zk.Event{}, nil, err}
			return
		}
		select {
		case e := <-childEventCh:
			if e.Type == zk.EventNodeChildrenChanged {
				newChildren, _, err := zw.client.Children(e.Path)
				if err != nil {
					respChan <- watchResponse{e, nil, err}
					return
				}

				if len(children) < len(newChildren) {
					// a node was added -- watch the new node
					for _, i := range newChildren {
						if !contains(children, i) {
							newNode := path.Join(e.Path, i)
							if key == prefix {
								// a new service was created under prefix
								go zw.watchDir(newNode, respChan)
								nodes, _, _ := zw.client.Children(newNode)
								for _, node := range nodes {
									n := path.Join(newNode, node)
									go zw.watchKey(n, respChan)
									s, _, err := zw.client.Get(n)
									e.Type = zk.EventNodeCreated
									respChan <- watchResponse{e, decode(s), err}
								}
							} else {
								go zw.watchKey(newNode, respChan)
								s, _, err := zw.client.Get(newNode)
								e.Type = zk.EventNodeCreated
								respChan <- watchResponse{e, decode(s), err}
							}
						}
					}
				}
			}
		case <-zw.stop:
			// There is no way to stop GetW/ChildrenW so just quit
			return
		}
	}
}

func (zw *zookeeperWatcher) watchKey(key string, respChan chan watchResponse) {
	for {
		s, _, keyEventCh, err := zw.client.GetW(key)
		if err != nil {
			respChan <- watchResponse{zk.Event{}, nil, err}
			return
		}

		select {
		case e := <-keyEventCh:
			switch e.Type {
			case zk.EventNodeDataChanged, zk.EventNodeCreated, zk.EventNodeDeleted:
				if e.Type != zk.EventNodeDeleted {
					// get the updated service
					s, _, err = zw.client.Get(e.Path)
				}
				respChan <- watchResponse{e, decode(s), err}
			}
			if e.Type == zk.EventNodeDeleted {
				//The Node was deleted - stop watching
				return
			}
		case <-zw.stop:
			// There is no way to stop GetW/ChildrenW so just quit
			return
		}
	}
}

func (zw *zookeeperWatcher) watch() {
	//get all Services
	services, _, err := zw.client.Children(prefix)
	if err != nil {
		zw.results <- result{nil, err}
	}
	respChan := make(chan watchResponse)

	//watch the prefix for new child nodes
	go zw.watchDir(prefix, respChan)

	//watch every service
	for _, service := range services {
		sPath := servicePath(service)
		go zw.watchDir(sPath, respChan)
		children, _, err := zw.client.Children(sPath)
		if err != nil {
			zw.results <- result{nil, err}
		}
		for _, c := range children {
			go zw.watchKey(path.Join(sPath, c), respChan)
		}
	}

	var service *registry.Service
	var action string
	for {
		select {
		case <-zw.stop:
			return
		case rsp := <-respChan:
			if rsp.err != nil {
				zw.results <- result{nil, err}
				continue
			}
			switch rsp.event.Type {
			case zk.EventNodeDataChanged:
				action = "update"
				service = rsp.service
			case zk.EventNodeDeleted:
				action = "delete"
				service = rsp.service
			case zk.EventNodeCreated:
				action = "create"
				service = rsp.service
			}
		}
		zw.results <- result{&registry.Result{Action: action, Service: service}, nil}
	}
}

func (zw *zookeeperWatcher) Stop() {
	zw.once.Do(func() {
		close(zw.stop)
	})
}

func (zw *zookeeperWatcher) Next() (*registry.Result, error) {
	select {
	case <-zw.stop:
		return nil, errors.New("watcher stopped")
	case r := <-zw.results:
		return r.res, r.err
	}
}
