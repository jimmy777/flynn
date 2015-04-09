package installer

import (
	"database/sql"
	"errors"
	"sync"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/aws"
	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
)

var ClusterNotFoundError = errors.New("Cluster not found")

type Installer struct {
	db            *sql.DB
	events        []*httpEvent
	subscriptions []*Subscription
	clusters      []*Cluster
	logger        log.Logger

	dbMtx        sync.Mutex
	eventsMtx    sync.Mutex
	subscribeMtx sync.Mutex
	clustersMtx  sync.Mutex
}

func NewInstaller(l log.Logger) *Installer {
	installer := &Installer{
		events:        make([]*httpEvent, 0),
		subscriptions: make([]*Subscription, 0),
		clusters:      make([]*Cluster, 0),
		logger:        l,
	}
	if err := installer.openDB(); err != nil {
		panic(err)
	}
	return installer
}

func (i *Installer) LaunchCluster(c *Cluster) error {
	if err := c.SetDefaultsAndValidate(); err != nil {
		return err
	}
	i.dbMtx.Lock()
	tx, err := i.db.Begin()
	if err != nil {
		i.dbMtx.Unlock()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO aws_clusters (cluster, region, num_instances, instance_type, vpc_cidr, subnet_cidr, stack_name) VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, c.Region, c.NumInstances, c.InstanceType, c.VpcCidr, c.SubnetCidr, c.StackName); err != nil {
		tx.Rollback()
		i.dbMtx.Unlock()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO clusters (id, aws_cluster, credential, state) VALUES (?, ?, ?, ?)`, c.ID, c.ID, c.CredentialID, c.State); err != nil {
		tx.Rollback()
		i.dbMtx.Unlock()
		return err
	}
	if err := tx.Commit(); err != nil {
		i.dbMtx.Unlock()
		return err
	}
	i.dbMtx.Unlock()
	i.clustersMtx.Lock()
	i.clusters = append(i.clusters, c)
	i.clustersMtx.Unlock()
	i.SendEvent(&httpEvent{
		Type:      "new_cluster",
		Cluster:   c,
		ClusterID: c.ID,
	})
	c.RunAWS()
	return nil
}

func (i *Installer) SaveAWSCredentials(id, secret string) error {
	i.dbMtx.Lock()
	defer i.dbMtx.Unlock()
	tx, err := i.db.Begin()
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO credentials (id, secret) VALUES (?, ?)`, id, secret); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (i *Installer) FindAWSCredentials(id string) (aws.CredentialsProvider, error) {
	if id == "aws_env" {
		return aws.EnvCreds()
	}
	var secret string
	if err := i.db.QueryRow(`SELECT secret FROM credentials WHERE id = ?`, id).Scan(&secret); err != nil {
		return nil, err
	}
	return aws.Creds(id, secret, ""), nil
}

func (i *Installer) FindCluster(id string) (*Cluster, error) {
	i.clustersMtx.Lock()
	for _, c := range i.clusters {
		if c.ID == id {
			i.clustersMtx.Unlock()
			return c, nil
		}
	}
	i.clustersMtx.Unlock()

	c := &Cluster{ID: id, installer: i}
	err := i.db.QueryRow(`SELECT credential, state, image, discovery_token, controller_key, controller_pin, dashboard_login_token, cert, region, num_instances, instance_type, vpc_cidr, subnet_cidr, stack_name, stack_id, dns_zone_id FROM clusters INNER JOIN aws_clusters WHERE aws_clusters.cluster = clusters.aws_cluster AND clusters.id = ? LIMIT 1`, c.ID).Scan(&c.CredentialID, &c.State, &c.ImageID, &c.DiscoveryToken, &c.ControllerKey, &c.ControllerPin, &c.DashboardLoginToken, &c.CACert, &c.Region, &c.NumInstances, &c.InstanceType, &c.VpcCidr, &c.SubnetCidr, &c.StackName, &c.StackID, &c.DNSZoneID)
	if err == sql.ErrNoRows {
		return nil, ClusterNotFoundError
	}
	if err != nil {
		return nil, err
	}
	if c.Creds, err = i.FindAWSCredentials(c.CredentialID); err != nil {
		return nil, err
	}
	domain := &Domain{}
	if err := i.db.QueryRow(`SELECT name, token FROM domains WHERE cluster = ?`, c.ID).Scan(&domain.Name, &domain.Token); err != nil {
		return nil, err
	}
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
	i.SendEvent(&httpEvent{ // TODO(jvatic): Send two events, one before cleanup and one after
		Type:      "cluster_deleted",
		ClusterID: id,
	})
	return nil
}
