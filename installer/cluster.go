package installer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/aws"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/gen/cloudformation"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/gen/ec2"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/gen/route53"
	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	cfg "github.com/flynn/flynn/cli/config"
	"github.com/flynn/flynn/pkg/awsutil"
	"github.com/flynn/flynn/pkg/etcdcluster"
	"github.com/flynn/flynn/pkg/sshkeygen"
	"github.com/flynn/flynn/util/release/types"
)

type Event struct {
	Description string
}

var DisallowedEC2InstanceTypes = []string{"t1.micro", "t2.micro", "t2.small", "m1.small"}
var DefaultInstanceType = "m3.medium"

type Cluster struct {
	ID    string `json:"id"`
	State string `json:"state"` // enum(starting, error, running, deleting)

	Region       string                  `json:"region,omitempty"`
	NumInstances int                     `json:"num_instances,omitempty"`
	InstanceType string                  `json:"instance_type,omitempty"`
	Creds        aws.CredentialsProvider `json:"-"`
	CredentialID string                  `json:"-"`

	ControllerKey       string  `json:"controller_key,omitempty"`
	ControllerPin       string  `json:"controller_pin,omitempty"`
	DashboardLoginToken string  `json:"dashboard_login_token,omitempty"`
	Domain              *Domain `json:"domain"`
	CACert              string  `json:"ca_cert"`

	ImageID        string                `json:"image_id,omitempty"`
	StackID        string                `json:"stack_id,omitempty"`
	StackName      string                `json:"stack_name,omitempty"`
	Stack          *cloudformation.Stack `json:"-"`
	SSHKey         *sshkeygen.SSHKey     `json:"-"`
	SSHKeyName     string                `json:"ssh_key_name,omitempty"`
	VpcCidr        string                `json:"vpc_cidr_block,omitempty"`
	SubnetCidr     string                `json:"subnet_cidr_block,omitempty"`
	DiscoveryToken string                `json:"discovery_token"`
	InstanceIPs    []string              `json:"instance_ips,omitempty"`
	DNSZoneID      string                `json:"dns_zone_id,omitempty"`

	cf  *cloudformation.CloudFormation
	ec2 *ec2.EC2

	installer     *Installer
	pendingPrompt *httpPrompt
	done          bool
}

func (c *Cluster) SetDefaultsAndValidate() error {
	if c.NumInstances == 0 {
		c.NumInstances = 1
	}

	if c.InstanceType == "" {
		c.InstanceType = DefaultInstanceType
	}

	if c.VpcCidr == "" {
		c.VpcCidr = "10.0.0.0/16"
	}

	if c.SubnetCidr == "" {
		c.SubnetCidr = "10.0.0.0/21"
	}

	if err := c.validateInputs(); err != nil {
		return err
	}

	c.InstanceIPs = make([]string, 0, c.NumInstances)
	c.ec2 = ec2.New(c.Creds, c.Region, nil)
	c.cf = cloudformation.New(c.Creds, c.Region, nil)
	return nil
}

func (c *Cluster) validateInputs() error {
	if c.NumInstances <= 0 {
		return fmt.Errorf("You must specify at least one instance")
	}

	if c.Region == "" {
		return fmt.Errorf("No region specified")
	}

	if c.NumInstances > 5 {
		return fmt.Errorf("Maximum of 5 instances exceeded")
	}

	if c.NumInstances == 2 {
		return fmt.Errorf("You must specify 1 or 3+ instances, not 2")
	}

	for _, t := range DisallowedEC2InstanceTypes {
		if c.InstanceType == t {
			return fmt.Errorf("Unsupported instance type %s", c.InstanceType)
		}
	}

	return nil
}

func (c *Cluster) StackAddCmd() (string, error) {
	if c.ControllerKey == "" || c.ControllerPin == "" || c.Domain == nil || c.Domain.Name == "" {
		return "", fmt.Errorf("Not enough data present")
	}
	return fmt.Sprintf("flynn cluster add -g %[1]s:2222 -p %[2]s default https://controller.%[1]s %[3]s", c.Domain.Name, c.ControllerPin, c.ControllerKey), nil
}

func (c *Cluster) ClusterConfig() *cfg.Cluster {
	return &cfg.Cluster{
		Name:    c.StackName,
		URL:     "https://controller." + c.Domain.Name,
		Key:     c.ControllerKey,
		GitHost: fmt.Sprintf("%s:2222", c.Domain.Name),
		TLSPin:  c.ControllerPin,
	}
}

func (c *Cluster) DashboardLoginMsg() (string, error) {
	if c.DashboardLoginToken == "" || c.Domain == nil || c.Domain.Name == "" {
		return "", fmt.Errorf("Not enough data present")
	}
	return fmt.Sprintf("The built-in dashboard can be accessed at http://dashboard.%s with login token %s", c.Domain.Name, c.DashboardLoginToken), nil
}

func (c *Cluster) saveFields(fields []string, values []interface{}) error {
	return c.saveFieldsForTable("clusters", fields, values)
}

func (c *Cluster) saveAWSFields(fields []string, values []interface{}) error {
	return c.saveFieldsForTable("aws_clusters", fields, values)
}

func (c *Cluster) saveFieldsForTable(table string, fields []string, values []interface{}) error {
	c.installer.dbMtx.Lock()
	defer c.installer.dbMtx.Unlock()
	tx, err := c.installer.db.Begin()
	if err != nil {
		tx.Rollback()
		return err
	}
	if len(fields) != len(values) {
		return fmt.Errorf("saveFields: fields len (%d) and values len (%d) mismatch", len(fields), len(values))
	}
	set := make([]string, 0, len(fields))
	for _, f := range fields {
		set = append(set, fmt.Sprintf("%s = ?", f))
	}
	clusterIDColumn := "id"
	if table != "clusters" {
		clusterIDColumn = "cluster"
	}
	values = append(values, c.ID)
	if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s WHERE %s = ?`, table, strings.Join(set, ", "), clusterIDColumn), values...); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) saveInstanceIPs() error {
	c.installer.dbMtx.Lock()
	defer c.installer.dbMtx.Unlock()
	tx, err := c.installer.db.Begin()
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, ip := range c.InstanceIPs {
		if _, err := tx.Exec(`INSERT INTO instance_ips (ip, cluster) VALUES (?, ?)`, ip, c.ID); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) saveDomain() error {
	c.installer.dbMtx.Lock()
	defer c.installer.dbMtx.Unlock()
	tx, err := c.installer.db.Begin()
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO domains (name, token, cluster) VALUES (?, ?, ?)`, c.Domain.Name, c.Domain.Token, c.ID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) setState(state string) {
	c.State = state
	c.sendEvent(&httpEvent{
		ClusterID:   c.ID,
		Type:        "cluster_state",
		Description: c.State,
	})
	if err := c.saveFields([]string{"state"}, []interface{}{c.State}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("setState error: %s", err.Error()))
	}
}

func (c *Cluster) RunAWS() {
	go func() {
		defer func() {
			c.done = true
			c.handleDone()
		}()

		steps := []func() error{
			c.createKeyPair,
			c.allocateDomain,
			c.fetchImageID,
			c.createStack,
			c.fetchStackOutputs,
			c.configureDNS,
			c.bootstrap,
		}

		for _, step := range steps {
			if err := step(); err != nil {
				c.setState("error")
				c.SendError(err)
				return
			}
		}

		c.setState("running")

		if err := c.configureCLI(); err != nil {
			c.SendInstallLogEvent(fmt.Sprintf("WARNING: Failed to configure CLI: %s", err))
		}
	}()
}

func (c *Cluster) DeleteAWS() {
	// TODO(jvatic): Send log events
	c.cf = cloudformation.New(c.Creds, c.Region, nil)
	go func() {
		c.setState("deleting")
		if err := c.cf.DeleteStack(&cloudformation.DeleteStackInput{
			StackName: aws.String(c.StackName),
		}); err != nil {
			c.setState("error")
			c.SendError(fmt.Errorf("Unable to delete stack %s: %s", c.StackName, err))
		}
	}()
}

func (c *Cluster) fetchImageID() (err error) {
	defer func() {
		if err == nil {
			return
		}
		c.SendInstallLogEvent(err.Error())
		if c.ImageID != "" {
			c.SendInstallLogEvent("Falling back to saved Image ID")
			err = nil
			return
		}
		return
	}()

	c.SendInstallLogEvent("Fetching image manifest")

	latestVersion, err := fetchLatestVersion()
	if err != nil {
		return err
	}
	var imageID string
	for _, i := range latestVersion.Images {
		if i.Region == c.Region {
			imageID = i.ID
			break
		}
	}
	if imageID == "" {
		return errors.New(fmt.Sprintf("No image found for region %s", c.Region))
	}
	c.ImageID = imageID
	if err := c.saveFields([]string{"image"}, []interface{}{c.ImageID}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving ImageID: %s", err.Error()))
	}
	return nil
}

func (c *Cluster) allocateDomain() error {
	c.SendInstallLogEvent("Allocating domain")
	domain, err := AllocateDomain()
	if err != nil {
		return err
	}
	c.Domain = domain
	if err := c.saveDomain(); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving Domain: %s", err.Error()))
	}
	return nil
}

func (c *Cluster) loadKeyPair(name string) error {
	keypair, err := loadSSHKey(name)
	if err != nil {
		return err
	}
	fingerprint, err := awsutil.FingerprintImportedKey(keypair.PrivateKey)
	if err != nil {
		return err
	}
	res, err := c.ec2.DescribeKeyPairs(&ec2.DescribeKeyPairsRequest{
		Filters: []ec2.Filter{
			{
				Name:   aws.String("fingerprint"),
				Values: []string{fingerprint},
			},
		},
	})
	if err != nil {
		return err
	}
	if len(res.KeyPairs) == 0 {
		return errors.New("No matching key found")
	}
	c.SSHKey = keypair
	for _, p := range res.KeyPairs {
		if *p.KeyName == name {
			c.SSHKeyName = name
			return nil
		}
	}
	c.SSHKeyName = *res.KeyPairs[0].KeyName
	return saveSSHKey(c.SSHKeyName, keypair)
}

func (c *Cluster) createKeyPair() error {
	keypairName := "flynn"
	if c.SSHKeyName != "" {
		keypairName = c.SSHKeyName
	}
	if err := c.loadKeyPair(keypairName); err == nil {
		c.SendInstallLogEvent(fmt.Sprintf("Using saved key pair (%s)", c.SSHKeyName))
		return nil
	}

	c.SendInstallLogEvent("Creating key pair")
	keypair, err := sshkeygen.Generate()
	if err != nil {
		return err
	}

	enc := base64.StdEncoding
	publicKeyBytes := make([]byte, enc.EncodedLen(len(keypair.PublicKey)))
	enc.Encode(publicKeyBytes, keypair.PublicKey)

	res, err := c.ec2.ImportKeyPair(&ec2.ImportKeyPairRequest{
		KeyName:           aws.String(keypairName),
		PublicKeyMaterial: publicKeyBytes,
	})
	if apiErr, ok := err.(aws.APIError); ok && apiErr.Code == "InvalidKeyPair.Duplicate" {
		if c.YesNoPrompt(fmt.Sprintf("Key pair %s already exists, would you like to delete it?", keypairName)) {
			c.SendInstallLogEvent("Deleting key pair")
			if err := c.ec2.DeleteKeyPair(&ec2.DeleteKeyPairRequest{
				KeyName: aws.String(keypairName),
			}); err != nil {
				return err
			}
			return c.createKeyPair()
		} else {
			for {
				keypairName = c.PromptInput("Please enter a new key pair name")
				if keypairName != "" {
					c.SSHKeyName = keypairName
					return c.createKeyPair()
				}
			}
		}
	}
	if err != nil {
		return err
	}

	c.SSHKey = keypair
	c.SSHKeyName = *res.KeyName

	err = saveSSHKey(keypairName, keypair)
	if err != nil {
		return err
	}
	return nil
}

type stackTemplateData struct {
	Instances           []struct{}
	DefaultInstanceType string
}

func (c *Cluster) createStack() error {
	c.SendInstallLogEvent("Generating start script")
	startScript, discoveryToken, err := genStartScript(c.NumInstances)
	if err != nil {
		return err
	}
	c.DiscoveryToken = discoveryToken
	if err := c.saveFields([]string{"discovery_token"}, []interface{}{c.DiscoveryToken}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving DiscoveryToken: %s", err.Error()))
	}

	var stackTemplateBuffer bytes.Buffer
	err = stackTemplate.Execute(&stackTemplateBuffer, &stackTemplateData{
		Instances:           make([]struct{}, c.NumInstances),
		DefaultInstanceType: DefaultInstanceType,
	})
	if err != nil {
		return err
	}
	stackTemplateString := stackTemplateBuffer.String()

	parameters := []cloudformation.Parameter{
		{
			ParameterKey:   aws.String("ImageId"),
			ParameterValue: aws.String(c.ImageID),
		},
		{
			ParameterKey:   aws.String("ClusterDomain"),
			ParameterValue: aws.String(c.Domain.Name),
		},
		{
			ParameterKey:   aws.String("KeyName"),
			ParameterValue: aws.String(c.SSHKeyName),
		},
		{
			ParameterKey:   aws.String("UserData"),
			ParameterValue: aws.String(startScript),
		},
		{
			ParameterKey:   aws.String("InstanceType"),
			ParameterValue: aws.String(c.InstanceType),
		},
		{
			ParameterKey:   aws.String("VpcCidrBlock"),
			ParameterValue: aws.String(c.VpcCidr),
		},
		{
			ParameterKey:   aws.String("SubnetCidrBlock"),
			ParameterValue: aws.String(c.SubnetCidr),
		},
	}

	stackEventsSince := time.Now()

	if c.StackID != "" && c.StackName != "" {
		if err := c.fetchStack(); err == nil && !strings.HasPrefix(*c.Stack.StackStatus, "DELETE") {
			if c.YesNoPrompt(fmt.Sprintf("Stack found from previous installation (%s), would you like to delete it? (a new one will be created either way)", c.StackName)) {
				c.SendInstallLogEvent(fmt.Sprintf("Deleting stack %s", c.StackName))
				if err := c.cf.DeleteStack(&cloudformation.DeleteStackInput{
					StackName: aws.String(c.StackName),
				}); err != nil {
					c.SendInstallLogEvent(fmt.Sprintf("Unable to delete stack %s: %s", c.StackName, err))
				}
			}
		}
	}

	c.SendInstallLogEvent("Creating stack")
	res, err := c.cf.CreateStack(&cloudformation.CreateStackInput{
		OnFailure:        aws.String("DELETE"),
		StackName:        aws.String(c.StackName),
		Tags:             []cloudformation.Tag{},
		TemplateBody:     aws.String(stackTemplateString),
		TimeoutInMinutes: aws.Integer(10),
		Parameters:       parameters,
	})
	if err != nil {
		return err
	}
	c.StackID = *res.StackID

	if err := c.saveAWSFields([]string{"stack_id"}, []interface{}{c.StackID}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving StackID: %s", err.Error()))
	}
	return c.waitForStackCompletion("CREATE", stackEventsSince)
}

func fetchLatestVersion() (*release.EC2Version, error) {
	client := &http.Client{}
	resp, err := client.Get("https://dl.flynn.io/ec2/images.json")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("Failed to fetch list of flynn images: %s", resp.Status))
	}
	dec := json.NewDecoder(resp.Body)
	manifest := release.EC2Manifest{}
	err = dec.Decode(&manifest)
	if err != nil {
		return nil, err
	}
	if len(manifest.Versions) == 0 {
		return nil, errors.New("No versions in manifest")
	}
	return manifest.Versions[0], nil
}

type StackEventSort []cloudformation.StackEvent

func (e StackEventSort) Len() int {
	return len(e)
}

func (e StackEventSort) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}

func (e StackEventSort) Less(i, j int) bool {
	return e[j].Timestamp.After(e[i].Timestamp)
}

func (c *Cluster) waitForStackCompletion(action string, after time.Time) error {
	stackID := aws.String(c.StackID)

	actionCompleteSuffix := "_COMPLETE"
	actionFailureSuffix := "_FAILED"
	isComplete := false
	isFailed := false

	stackEvents := make([]cloudformation.StackEvent, 0)
	var nextToken aws.StringValue

	var fetchStackEvents func() error
	fetchStackEvents = func() error {
		res, err := c.cf.DescribeStackEvents(&cloudformation.DescribeStackEventsInput{
			NextToken: nextToken,
			StackName: stackID,
		})
		if err != nil {
			switch err.(type) {
			case *url.Error:
				return nil
			default:
				return err
			}
		}

		// some events are not returned in order
		sort.Sort(StackEventSort(res.StackEvents))

		for _, se := range res.StackEvents {
			if !se.Timestamp.After(after) {
				continue
			}
			stackEventExists := false
			for _, e := range stackEvents {
				if *e.EventID == *se.EventID {
					stackEventExists = true
					break
				}
			}
			if stackEventExists {
				continue
			}
			stackEvents = append(stackEvents, se)
			if se.ResourceType != nil && se.ResourceStatus != nil {
				if *se.ResourceType == "AWS::CloudFormation::Stack" {
					if strings.HasSuffix(*se.ResourceStatus, actionCompleteSuffix) {
						if strings.HasPrefix(*se.ResourceStatus, action) {
							isComplete = true
						} else {
							isFailed = true
						}
					} else if strings.HasSuffix(*se.ResourceStatus, actionFailureSuffix) {
						isFailed = true
					}
				}
				var desc string
				if se.ResourceStatusReason != nil {
					desc = fmt.Sprintf(" (%s)", *se.ResourceStatusReason)
				}
				name := *se.ResourceType
				if se.LogicalResourceID != nil {
					name = fmt.Sprintf("%s (%s)", name, *se.LogicalResourceID)
				}
				c.SendInstallLogEvent(fmt.Sprintf("%s\t%s%s", name, *se.ResourceStatus, desc))
			}
		}
		if res.NextToken != nil {
			nextToken = res.NextToken
			fetchStackEvents()
		}

		return nil
	}

	for {
		if err := fetchStackEvents(); err != nil {
			return err
		}
		if isComplete {
			break
		}
		if isFailed {
			return fmt.Errorf("Failed to create stack %s", c.StackName)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

func (c *Cluster) fetchStack() error {
	stackID := aws.String(c.StackID)

	c.SendInstallLogEvent("Fetching stack")
	res, err := c.cf.DescribeStacks(&cloudformation.DescribeStacksInput{
		StackName: stackID,
	})
	if err != nil {
		return err
	}
	if len(res.Stacks) == 0 {
		return errors.New("Stack does not exist")
	}
	stack := &res.Stacks[0]
	if strings.HasPrefix(*stack.StackStatus, "DELETE_") {
		return fmt.Errorf("Stack in unusable state: %s", *stack.StackStatus)
	}
	c.Stack = stack
	return nil
}

func (c *Cluster) fetchStackOutputs() error {
	c.fetchStack()

	c.InstanceIPs = make([]string, 0, c.NumInstances)
	for _, o := range c.Stack.Outputs {
		v := *o.OutputValue
		if strings.HasPrefix(*o.OutputKey, "IPAddress") {
			c.InstanceIPs = append(c.InstanceIPs, v)
		}
		if *o.OutputKey == "DNSZoneID" {
			c.DNSZoneID = v
		}
	}
	if len(c.InstanceIPs) != c.NumInstances {
		return fmt.Errorf("expected stack outputs to include %d instance IPs but found %d", c.NumInstances, len(c.InstanceIPs))
	}
	if c.DNSZoneID == "" {
		return fmt.Errorf("stack outputs do not include DNSZoneID")
	}

	if err := c.saveAWSFields([]string{"dns_zone_id"}, []interface{}{c.DNSZoneID}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving DNSZoneID: %s", err.Error()))
	}

	if err := c.saveInstanceIPs(); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving InstanceIPs: %s", err.Error()))
	}

	return nil
}

func (c *Cluster) configureDNS() error {
	// TODO(jvatic): Run directly after receiving zone create complete stack event
	c.SendInstallLogEvent("Configuring DNS")

	// Set region to us-east-1, since any other region will fail for global services like Route53
	r53 := route53.New(c.Creds, "us-east-1", nil)
	res, err := r53.GetHostedZone(&route53.GetHostedZoneRequest{ID: aws.String(c.DNSZoneID)})
	if err != nil {
		return err
	}
	if err := c.Domain.Configure(res.DelegationSet.NameServers); err != nil {
		return err
	}

	return nil
}

func (c *Cluster) waitForDNS() error {
	c.SendInstallLogEvent("Waiting for DNS to propagate")
	for {
		status, err := c.Domain.Status()
		if err != nil {
			return err
		}
		if status == "applied" {
			break
		}
		time.Sleep(time.Second)
	}
	c.SendInstallLogEvent("DNS is live")
	return nil
}

func instanceRunCmd(cmd string, sshConfig *ssh.ClientConfig, ipAddress string) (stdout, stderr io.Reader, err error) {
	var sshConn *ssh.Client
	sshConn, err = ssh.Dial("tcp", ipAddress+":22", sshConfig)
	if err != nil {
		return
	}
	defer sshConn.Close()

	sess, err := sshConn.NewSession()
	if err != nil {
		return
	}
	stdout, err = sess.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err = sess.StderrPipe()
	if err != nil {
		return
	}
	if err = sess.Start(cmd); err != nil {
		return
	}

	err = sess.Wait()
	return
}

func (c *Cluster) uploadDebugInfo(sshConfig *ssh.ClientConfig, ipAddress string) {
	cmd := "sudo flynn-host collect-debug-info"
	stdout, stderr, _ := instanceRunCmd(cmd, sshConfig, ipAddress)
	var buf bytes.Buffer
	io.Copy(&buf, stdout)
	io.Copy(&buf, stderr)
	c.SendInstallLogEvent(fmt.Sprintf("`%s` output for %s: %s", cmd, ipAddress, buf.String()))
}

type stepInfo struct {
	ID        string           `json:"id"`
	Action    string           `json:"action"`
	Data      *json.RawMessage `json:"data"`
	State     string           `json:"state"`
	Error     string           `json:"error,omitempty"`
	Timestamp time.Time        `json:"ts"`
}

func (c *Cluster) bootstrap() error {
	c.SendInstallLogEvent("Running bootstrap")

	if c.Stack == nil {
		return errors.New("No stack found")
	}

	if c.SSHKey == nil {
		return errors.New("No SSHKey found")
	}

	// bootstrap only needs to run on one instance
	ipAddress := c.InstanceIPs[0]

	signer, err := ssh.NewSignerFromKey(c.SSHKey.PrivateKey)
	if err != nil {
		return err
	}
	sshConfig := &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	}

	attempts := 0
	maxAttempts := 3
	var sshConn *ssh.Client
	for {
		sshConn, err = ssh.Dial("tcp", ipAddress+":22", sshConfig)
		if err != nil {
			if attempts < maxAttempts {
				attempts += 1
				time.Sleep(time.Second)
				continue
			}
			return err
		}
		break
	}
	defer sshConn.Close()

	sess, err := sshConn.NewSession()
	if err != nil {
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	sess.Stderr = os.Stderr
	if err := sess.Start(fmt.Sprintf("CLUSTER_DOMAIN=%s flynn-host bootstrap --json", c.Domain.Name)); err != nil {
		c.uploadDebugInfo(sshConfig, ipAddress)
		return err
	}

	var keyData struct {
		Key string `json:"data"`
	}
	var loginTokenData struct {
		Token string `json:"data"`
	}
	var controllerCertData struct {
		Pin    string `json:"pin"`
		CACert string `json:"ca_cert"`
	}
	output := json.NewDecoder(stdout)
	for {
		var stepRaw json.RawMessage
		if err := output.Decode(&stepRaw); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		var step stepInfo
		if err := json.Unmarshal(stepRaw, &step); err != nil {
			return err
		}
		if step.State == "error" {
			c.uploadDebugInfo(sshConfig, ipAddress)
			return fmt.Errorf("bootstrap: %s %s error: %s", step.ID, step.Action, step.Error)
		}
		c.SendInstallLogEvent(fmt.Sprintf("%s: %s", step.ID, step.State))
		if step.State != "done" {
			continue
		}
		switch step.ID {
		case "controller-key":
			if err := json.Unmarshal(*step.Data, &keyData); err != nil {
				return err
			}
		case "controller-cert":
			if err := json.Unmarshal(*step.Data, &controllerCertData); err != nil {
				return err
			}
		case "dashboard-login-token":
			if err := json.Unmarshal(*step.Data, &loginTokenData); err != nil {
				return err
			}
		case "log-complete":
			break
		}
	}
	if keyData.Key == "" || controllerCertData.Pin == "" {
		return err
	}

	c.ControllerKey = keyData.Key
	c.ControllerPin = controllerCertData.Pin
	c.CACert = controllerCertData.CACert
	c.DashboardLoginToken = loginTokenData.Token

	if err := c.saveFields([]string{"controller_key", "controller_pin", "cert", "dashboard_login_token"}, []interface{}{
		c.ControllerKey,
		c.ControllerPin,
		c.CACert,
		c.DashboardLoginToken,
	}); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("Error saving ImageID: %s", err.Error()))
	}

	if err := sess.Wait(); err != nil {
		return err
	}
	if err := c.waitForDNS(); err != nil {
		return err
	}

	return nil
}

func (c *Cluster) configureCLI() error {
	config, err := cfg.ReadFile(cfg.DefaultPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := config.Add(c.ClusterConfig(), true); err != nil {
		return err
	}
	config.SetDefault(c.StackName)
	if err := config.SaveTo(cfg.DefaultPath()); err != nil {
		return err
	}
	c.SendInstallLogEvent("CLI configured locally")
	return nil
}

func genStartScript(nodes int) (string, string, error) {
	var data struct {
		DiscoveryToken string
	}
	var err error
	data.DiscoveryToken, err = etcdcluster.NewDiscoveryToken(strconv.Itoa(nodes))
	if err != nil {
		return "", "", err
	}
	buf := &bytes.Buffer{}
	w := base64.NewEncoder(base64.StdEncoding, buf)
	err = startScript.Execute(w, data)
	w.Close()

	return buf.String(), data.DiscoveryToken, err
}

var startScript = template.Must(template.New("start.sh").Parse(`
#!/bin/sh

# wait for libvirt
while ! [ -e /var/run/libvirt/libvirt-sock ]; do
  sleep 0.1
done

flynn-host init --discovery={{.DiscoveryToken}}
start flynn-host
`[1:]))

func (c *Cluster) GetOutput(name string) (string, error) {
	var value string
	for _, o := range c.Stack.Outputs {
		if *o.OutputKey == name {
			value = *o.OutputValue
			break
		}
	}
	if value == "" {
		return "", fmt.Errorf("stack outputs do not include %s", name)
	}
	return value, nil
}
