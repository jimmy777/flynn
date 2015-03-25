package installer

import (
	"errors"
	"sync"

	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
)

type Installer struct {
	Clusters []*Cluster `json:"clusters"`

	events        []*httpEvent
	subscriptions []*Subscription

	eventChan chan *httpEvent
	errChan   chan error

	logger log.Logger

	persistMutex sync.Mutex
	subscribeMtx sync.Mutex
	eventsMtx    sync.Mutex
}

func NewInstaller(l log.Logger) *Installer {
	installer := &Installer{
		events:        make([]*httpEvent, 0),
		subscriptions: make([]*Subscription, 0),
		logger:        l,
	}
	installer.load()
	go installer.handleEvents()
	return installer
}

func (i *Installer) LaunchCluster(c *Cluster) error {
	i.persistMutex.Lock()
	defer i.persistMutex.Unlock()
	i.SendEvent(&httpEvent{
		Type:      "new_cluster",
		Cluster:   c,
		ClusterID: c.ID,
	})
	if err := c.RunAWS(); err != nil {
		return err
	}
	i.Clusters = append(i.Clusters, c)
	return nil
}

func (i *Installer) FindCluster(id string) (*Cluster, error) {
	i.persistMutex.Lock()
	defer i.persistMutex.Unlock()
	for _, c := range i.Clusters {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, errors.New("Cluster not found")
}

func (i *Installer) DeleteCluster(id string) {
	i.persistMutex.Lock()
	defer i.persistMutex.Unlock()
	clusters := make([]*Cluster, 0, len(i.Clusters))
	var cluster *Cluster
	for _, c := range i.Clusters {
		if c.ID == id {
			cluster = c
		} else {
			clusters = append(clusters, c)
		}
	}
	if cluster != nil {
		cluster.DeleteAWS()
	}
	i.Clusters = clusters
	i.SendEvent(&httpEvent{ // TODO(jvatic): Send two events, one before cleanup and one after
		Type:      "cluster_deleted",
		ClusterID: id,
	})
}
