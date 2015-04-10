package installer

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/cznic/ql"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/aws"
	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
)

var ClusterNotFoundError = errors.New("Cluster not found")

type Installer struct {
	db            *ql.DB
	events        []*Event
	subscriptions []*Subscription
	clusters      []interface{}
	logger        log.Logger

	dbMtx        sync.RWMutex
	eventsMtx    sync.Mutex
	subscribeMtx sync.Mutex
	clustersMtx  sync.RWMutex
}

func NewInstaller(l log.Logger) *Installer {
	installer := &Installer{
		events:        make([]*Event, 0),
		subscriptions: make([]*Subscription, 0),
		clusters:      make([]interface{}, 0),
		logger:        l,
	}
	if err := installer.openDB(); err != nil {
		panic(err)
	}
	return installer
}

func (i *Installer) LaunchCluster(c interface{}) error {
	switch v := c.(type) {
	case *AWSCluster:
		return i.launchAWSCluster(v)
	default:
		return fmt.Errorf("Invalid cluster type %T", c)
	}
}

func (i *Installer) launchAWSCluster(c *AWSCluster) error {
	if err := c.SetDefaultsAndValidate(); err != nil {
		return err
	}

	if err := i.saveAWSCluster(c); err != nil {
		return err
	}

	i.clustersMtx.Lock()
	i.clusters = append(i.clusters, c)
	i.clustersMtx.Unlock()
	i.SendEvent(&Event{
		Type:      "new_cluster",
		Cluster:   c.cluster,
		ClusterID: c.cluster.ID,
	})
	c.Run()
	return nil
}

func (i *Installer) saveAWSCluster(c *AWSCluster) error {
	i.dbMtx.Lock()
	defer i.dbMtx.Unlock()

	clusterFields, err := ql.Marshal(c.cluster)
	if err != nil {
		return err
	}
	awsFields, err := ql.Marshal(c)
	if err != nil {
		return err
	}
	clustersVStr := make([]string, 0, len(clusterFields))
	awsVStr := make([]string, 0, len(awsFields))
	fields := make([]interface{}, 0, len(clusterFields)+len(awsFields))
	for idx, f := range clusterFields {
		clustersVStr = append(clustersVStr, fmt.Sprintf("$%d", idx+1))
		fields = append(fields, f)
	}
	for idx, f := range awsFields {
		awsVStr = append(awsVStr, fmt.Sprintf("$%d", idx+1))
		fields = append(fields, f)
	}

	ctx := ql.NewRWCtx()
	list, err := ql.Compile(fmt.Sprintf(`
	BEGIN TRANSACTION;
		INSERT INTO clusters VALUES (%s);
		INSERT INTO aws_clusters VALUES(%s);
	COMMIT;`, strings.Join(clustersVStr, ", "), strings.Join(awsVStr, ", ")))
	if err != nil {
		return err
	}
	_, _, err = i.db.Execute(ctx, list, fields...)
	if err != nil {
		return err
	}

	return nil
}

func (i *Installer) SaveAWSCredentials(id, secret string) error {
	i.dbMtx.Lock()
	defer i.dbMtx.Unlock()

	ctx := ql.NewRWCtx()
	list, err := ql.Compile(`
	BEGIN TRANSACTION;
		INSERT INTO credentials (ID, Secret) VALUES ($1, $2);
	COMMIT;`)
	if err != nil {
		return err
	}
	_, _, err = i.db.Execute(ctx, list, id, secret)
	if err != nil {
		return err
	}

	return nil
}

func (i *Installer) FindAWSCredentials(id string) (aws.CredentialsProvider, error) {
	if id == "aws_env" {
		return aws.EnvCreds()
	}
	var secret string

	// TODO(jvatic): Fetch credential from db

	return aws.Creds(id, secret, ""), nil
}

func (i *Installer) FindCluster(id string) (*Cluster, error) {
	i.clustersMtx.RLock()
	for _, c := range i.clusters {
		v := reflect.Indirect(reflect.ValueOf(c))
		cluster := v.FieldByName("cluster").Interface().(*Cluster)
		if cluster.ID == id {
			i.clustersMtx.RUnlock()
			return cluster, nil
		}
	}
	i.clustersMtx.RUnlock()

	i.dbMtx.RLock()
	defer i.dbMtx.RUnlock()

	c := &Cluster{ID: id, installer: i}

	// TODO(jvatic): Fetch cluster from db

	if c.Creds, err = i.FindAWSCredentials(c.CredentialID); err != nil {
		return nil, err
	}
	domain := &Domain{}

	// TODO(jvatic): Fetch domain from db

	c.Domain = domain
	return c, nil
}

func (i *Installer) DeleteCluster(id string) error {
	i.dbMtx.Lock()
	cluster, err := i.FindCluster(id)
	i.dbMtx.Unlock()
	if err != nil {
		return err
	}

	i.clustersMtx.Lock()
	clusters := make([]*Cluster, 0, len(i.clusters))
	for _, c := range i.clusters {
		if c.ID != id {
			clusters = append(clusters, c)
		}
	}
	i.clusters = clusters
	i.clustersMtx.Unlock()

	// TODO(jvatic): remove from database once stack deletion complete
	cluster.DeleteAWS()
	i.SendEvent(&Event{ // TODO(jvatic): Send two events, one before cleanup and one after
		Type:      "cluster_deleted",
		ClusterID: id,
	})
	return nil
}
