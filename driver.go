package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	linstor "github.com/LINBIT/golinstor"
	"github.com/LINBIT/golinstor/client"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/docker/go-plugins-helpers/volume"
	"github.com/mitchellh/mapstructure"
	"github.com/rck/unit"
	"github.com/vrischmann/envconfig"
	"gopkg.in/ini.v1"
	"k8s.io/kubernetes/pkg/util/mount"
	mountutils "k8s.io/mount-utils"
	"k8s.io/utils/exec"
)

const (
	datadir         = "data"
	pluginFlagKey   = "Aux/is-linstor-docker-volume"
	pluginFlagValue = "true"
	pluginFSTypeKey = "FileSystem/Type"
)

type LinstorConfig struct {
	Controllers string
	Username    string
	Password    string
	CertFile    string
	KeyFile     string
	CAFile      string
}

type LinstorParams struct {
	Nodes               []string `mapstructure:"nodes"`
	ReplicasOnDifferent []string `mapstructure:"replicas-on-different"`
	ReplicasOnSame      []string `mapstructure:"replicas-on-same"`
	DisklessStoragePool string   `mapstructure:"diskless-storage-pool"`
	DoNotPlaceWithRegex string   `mapstructure:"do-not-place-with-regex"`
	FS                  string   `mapstructure:"fs"`
	FSOpts              string   `mapstructure:"fsopts"`
	MountOpts           []string `mapstructure:"mount-opts"`
	StoragePool         string   `mapstructure:"storage-pool"`
	Size                string   `mapstructure:"size"`
	SizeKiB             uint64
	Replicas            int32    `mapstructure:"replicas"`
	DisklessOnRemaining bool     `mapstructure:"diskless-on-remaining"`

	// DRBD options from docker-volume.conf [global]
	Protocol              string `mapstructure:"protocol"`
	ConnectInterval       string `mapstructure:"connect-int"`
	PingInterval          string `mapstructure:"ping-int"`
	PingTimeout           string `mapstructure:"ping-timeout"`
	ResyncRate            string `mapstructure:"resync-rate"`
	ALExtents             string `mapstructure:"al-extents"`
	MaxBuffers            string `mapstructure:"max-buffers"`
	MaxEpochSize          string `mapstructure:"max-epoch-size"`
	HandlerSplitBrain     string `mapstructure:"handler-split-brain"`
	HandlerPriOnInconDegr string `mapstructure:"handler-pri-on-incon-degr"`
	PrimarySetOn          string `mapstructure:"primary-set-on"`
}

type LinstorDriver struct {
	config  string
	node    string
	root    string
	mounter *mount.SafeFormatAndMount
	resizer *mountutils.ResizeFs
}

func NewLinstorDriver(config, node, root string) *LinstorDriver {
	return &LinstorDriver{
		config: config,
		node:   node,
		root:   root,
		mounter: &mount.SafeFormatAndMount{
			Interface: mount.New("/bin/mount"),
			Exec:      mount.NewOsExec(),
		},
		resizer: mountutils.NewResizeFs(exec.New()),
	}
}

func (l *LinstorDriver) newBaseURL(hosts string) (*url.URL, error) {
	scheme := "http"
	host := "localhost:3370"
	if hosts != "" {
		parts := strings.SplitN(hosts, ",", 2)
		h := parts[0]
		if p := strings.SplitN(h, "://", 2); len(p) == 2 {
			if p[0] == "linstor+ssl" || p[0] == "https" {
				scheme = "https"
			}
			host = p[1]
		}
	}

	if !strings.Contains(host, ":") {
		switch scheme {
		case "http": host += ":3370"
		case "https": host += ":3371"
		}
	}
	return url.Parse(scheme + "://" + host)
}

func (l *LinstorDriver) newClient() (*client.Client, error) {
	config := new(LinstorConfig)
	if err := l.loadConfig(config); err != nil {
		return nil, err
	}

	err := envconfig.InitWithOptions(config, envconfig.Options{Prefix: "LS", AllOptional: true})
	if err != nil {
		return nil, err
	}

	baseURL, err := l.newBaseURL(config.Controllers)
	if err != nil {
		return nil, err
	}

	tlsConfig, err := tlsconfig.Client(tlsconfig.Options{
		CertFile:           config.CertFile,
		KeyFile:            config.KeyFile,
		CAFile:             config.CAFile,
		InsecureSkipVerify: config.CAFile == "",
		ExclusiveRootPools: true,
	})
	if err != nil {
		return nil, err
	}

	return client.NewClient(
		client.BaseURL(baseURL),
		client.BasicAuth(&client.BasicAuthCfg{Username: config.Username, Password: config.Password}),
		client.HTTPClient(&http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}),
	)
}

func (l *LinstorDriver) newParams(name string, options map[string]string) (*LinstorParams, error) {
	params := new(LinstorParams)
	if err := l.loadConfig(params); err != nil {
		return nil, err
	}
	if options != nil {
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{Result: params, WeaklyTypedInput: true, DecodeHook: mapstructure.StringToSliceHookFunc(" ")})
		if err != nil {
			return nil, err
		}
		if err = decoder.Decode(options); err != nil {
			return nil, err
		}
	}
	// size conversion
	if params.Size == "" { params.Size = "100MB" }
	u := unit.MustNewUnit(unit.DefaultUnits)
	v, err := u.ValueFromString(params.Size)
	if err != nil { return nil, fmt.Errorf("Could not convert '%s': %v", params.Size, err) }
	bytes := v.Value; lower := 4 * unit.M
	if bytes < lower { bytes = lower }
	params.SizeKiB = uint64(bytes / unit.K)
	if params.FS == "" { params.FS = "ext4" }
	if params.Replicas == 0 { params.Replicas = 2 }
	return params, nil
}

func (l *LinstorDriver) Create(req *volume.CreateRequest) error {
	params, err := l.newParams(req.Name, req.Options)
	if err != nil { return err }
	c, err := l.newClient()
	if err != nil { return err }
	ctx := context.Background()

	// volume definition (size)
	if err := c.ResourceDefinitions.CreateVolumeDefinition(ctx, req.Name, client.VolumeDefinitionCreate{VolumeDefinition: client.VolumeDefinition{SizeKib: params.SizeKiB}}); err != nil {
		return err
	}

	// build props
	props := map[string]string{pluginFlagKey: pluginFlagValue, pluginFSTypeKey: params.FS, "FileSystem/MkfsParams": params.FSOpts}
	addProp := func(key, val string) { if val != "" { props["drbdOptions/"+key] = val } }
	addProp("protocol", params.Protocol)
	addProp("connect-int", params.ConnectInterval)
	addProp("ping-int", params.PingInterval)
	addProp("ping-timeout", params.PingTimeout)
	addProp("resync-rate", params.ResyncRate)
	addProp("al-extents", params.ALExtents)
	addProp("max-buffers", params.MaxBuffers)
	addProp("max-epoch-size", params.MaxEpochSize)
	addProp("handler-split-brain", params.HandlerSplitBrain)
	addProp("handler-pri-on-incon-degr", params.HandlerPriOnInconDegr)
	addProp("primary-set-on", params.PrimarySetOn)

	// resource definition
	if err := c.ResourceDefinitions.Create(ctx, client.ResourceDefinitionCreate{ResourceDefinition: client.ResourceDefinition{Name: req.Name, Props: props}}); err != nil {
		c.ResourceDefinitions.DeleteVolumeDefinition(ctx, req.Name, 0)
		return err
	}

	// place resources
	if err := l.resourcesCreate(ctx, c, req, params); err != nil {
		c.ResourceDefinitions.Delete(ctx, req.Name)
		c.ResourceDefinitions.DeleteVolumeDefinition(ctx, req.Name, 0)
		return err
	}
	return nil
}

// resourcesCreate places diskfull or diskless based on params
func (l *LinstorDriver) resourcesCreate(ctx context.Context, c *client.Client, req *volume.CreateRequest, params *LinstorParams) error {
	err := c.ResourceDefinitions.Create(ctx, client.ResourceDefinitionCreate{ /* noop: skip */ }) // placeholder
	// original logic here
	if len(params.Nodes) == 0 {
		return c.Resources.Autoplace(ctx, req.Name, client.AutoPlaceRequest{
			DisklessOnRemaining: params.DisklessOnRemaining,
			SelectFilter: client.AutoSelectFilter{PlaceCount: params.Replicas, StoragePool: params.StoragePool, NotPlaceWithRscRegex: params.DoNotPlaceWithRegex, ReplicasOnSame: params.ReplicasOnSame, ReplicasOnDifferent: params.ReplicasOnDifferent},
		})
	}
	for _, node := range params.Nodes {
		if err := c.Resources.Create(ctx, l.toDiskfullCreate(req.Name, node, params)); err != nil {
			return err
		}
	}
	return err
}

func (l *LinstorDriver) Get(req *volume.GetRequest) (*volume.GetResponse, error) {
	c, err := l.newClient()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	resourceDef, err := c.ResourceDefinitions.Get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if resourceDef.Props[pluginFlagKey] != pluginFlagValue {
		return nil, fmt.Errorf("Volume '%s' is not managed by this plugin", req.Name)
	}
	vol := &volume.Volume{
		Name:       resourceDef.Name,
		Mountpoint: l.mountPoint(resourceDef.Name),
	}
	return &volume.GetResponse{vol}, nil
}

func (l *LinstorDriver) List() (*volume.ListResponse, error) {
	c, err := l.newClient()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	resourceDefs, err := c.ResourceDefinitions.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	vols := []*volume.Volume{}
	for _, resourceDef := range resourceDefs {
		if resourceDef.Props[pluginFlagKey] != pluginFlagValue {
			continue
		}
		vols = append(vols, &volume.Volume{
			Name:       resourceDef.Name,
			Mountpoint: l.mountPoint(resourceDef.Name),
		})
	}
	return &volume.ListResponse{Volumes: vols}, nil
}

func (l *LinstorDriver) Remove(req *volume.RemoveRequest) error {
	return l.remove(req.Name, true)
}

func (l *LinstorDriver) Path(req *volume.PathRequest) (*volume.PathResponse, error) {
	return &volume.PathResponse{Mountpoint: l.mountPoint(req.Name)}, nil
}

func (l *LinstorDriver) Mount(req *volume.MountRequest) (*volume.MountResponse, error) {
	params, err := l.newParams(req.Name, nil)
	if err != nil {
		return nil, err
	}
	c, err := l.newClient()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if _, err = c.Resources.Get(ctx, req.Name, l.node); err == client.NotFoundError {
		err = c.Resources.Create(ctx, l.toDisklessCreate(req.Name, l.node, params))
		if err != nil {
			return nil, err
		}
	}
	// properties are not merged, so we have to query the resdef
	// as we set the property there
	resdef, err := c.ResourceDefinitions.Get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	fstype, ok := resdef.Props[pluginFSTypeKey]
	if !ok {
		return nil, fmt.Errorf("Volume '%s' did not contain a file system key", req.Name)
	}
	vol, err := c.Resources.GetVolume(ctx, req.Name, l.node, 0)
	if err != nil {
		return nil, err
	}
	source := vol.DevicePath
	inUse, err := l.mounter.DeviceOpened(source)
	if err != nil {
		return nil, err
	}
	if inUse {
		return nil, fmt.Errorf("unable to get exclusive open on %s", source)
	}
	target := l.realMountPath(req.Name)
	if err = l.mounter.MakeDir(target); err != nil {
		return nil, err
	}
	err = l.mounter.Mount(source, target, fstype, params.MountOpts)
	if err != nil {
		return nil, err
	}

	mnt := l.reportedMountPath(req.Name)
	if _, err = os.Stat(mnt); os.IsNotExist(err) { // check for remount
		if err = l.mounter.MakeDir(mnt); err != nil {
			return nil, err
		}
	}

	needResize, err := l.resizer.NeedResize(source, target)
	if err != nil {
		return nil, err
	}
	if needResize {
		if _, err = l.resizer.Resize(source, target); err != nil {
			return nil, err
		}
	}

	return &volume.MountResponse{Mountpoint: mnt}, nil
}

func (l *LinstorDriver) Unmount(req *volume.UnmountRequest) error {
	target := l.realMountPath(req.Name)
	notMounted, err := l.mounter.IsNotMountPoint(target)
	if err != nil || notMounted {
		return err
	}
	if err = l.mounter.Unmount(target); err != nil {
		return err
	}

	// try to remove now unused dir
	_ = os.Remove(target)

	diskless, err := l.isDiskless(req.Name)
	// in this case we don't really care about the error, just log it, and keep the diskless assignment.
	if err != nil {
		log.Println(err)
	} else if diskless {
		return l.remove(req.Name, false)
	}

	return nil
}

func (l *LinstorDriver) Capabilities() *volume.CapabilitiesResponse {
	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{Scope: "global"},
	}
}

func (l *LinstorDriver) loadConfig(result interface{}) error {
	if _, err := os.Stat(l.config); os.IsNotExist(err) {
		return nil
	}
	file, err := ini.InsensitiveLoad(l.config)
	if err != nil {
		return err
	}
	return file.Section("global").MapTo(result)
}

func (l *LinstorDriver) realMountPath(name string) string {
	return filepath.Join(l.root, name)
}

func (l *LinstorDriver) reportedMountPath(name string) string {
	return filepath.Join(l.realMountPath(name), datadir)
}

func (l *LinstorDriver) mountPoint(name string) string {
	path := l.realMountPath(name)
	notMounted, err := l.mounter.IsNotMountPoint(path)
	if err != nil || notMounted {
		return ""
	}
	return l.reportedMountPath(name)
}

func (l *LinstorDriver) toDiskfullCreate(name, node string, params *LinstorParams) client.ResourceCreate {
	props := make(map[string]string)
	if params.StoragePool != "" {
		props[linstor.KeyStorPoolName] = params.StoragePool
	}
	return client.ResourceCreate{
		Resource: client.Resource{
			Name:     name,
			NodeName: node,
			Props:    props,
		},
	}
}

func (l *LinstorDriver) toDisklessCreate(name, node string, params *LinstorParams) client.ResourceCreate {
	props := make(map[string]string)
	if params.DisklessStoragePool != "" {
		props[linstor.KeyStorPoolName] = params.DisklessStoragePool
	}
	return client.ResourceCreate{
		Resource: client.Resource{
			Name:     name,
			NodeName: node,
			Props:    props,
			Flags:    []string{linstor.FlagDiskless},
		},
	}
}

func (l *LinstorDriver) isDiskless(name string) (bool, error) {
	lopt := client.ListOpts{Resource: []string{name}, Node: []string{l.node}}
	c, err := l.newClient()
	if err != nil {
		return false, err
	}
	ctx := context.Background()

	// view to get storage information as well
	resources, err := c.Resources.GetResourceView(ctx, &lopt)
	if err != nil {
		return false, err
	}
	if len(resources) != 1 {
		return false, errors.New("Resource filter has to contain exactly one resource")
	}
	if len(resources[0].Volumes) != 1 {
		return false, errors.New("There has to be exactly one volume in the resource")
	}

	return resources[0].Volumes[0].ProviderKind == client.DISKLESS, nil
}

func (l *LinstorDriver) remove(name string, global bool) error {
	c, err := l.newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()

	if !global {
		return c.Resources.Delete(ctx, name, l.node)
	}

	// global
	snaps, err := c.Resources.GetSnapshots(ctx, name)
	if err != nil {
		return err
	}
	for _, snap := range snaps {
		err = c.Resources.DeleteSnapshot(ctx, name, snap.Name)
		if err != nil {
			return err
		}
	}
	return c.ResourceDefinitions.Delete(ctx, name)
}
