package gcp

import (
	"context"
	"strings"

	"github.com/projectdiscovery/cloudlist/pkg/schema"
	"github.com/projectdiscovery/gologger"
	errorutil "github.com/projectdiscovery/utils/errors"
	"google.golang.org/api/cloudfunctions/v1"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1beta1"
	"google.golang.org/api/dns/v1"
	run "google.golang.org/api/run/v1"
	"google.golang.org/api/storage/v1"
)

// Provider is a data provider for gcp API
type Provider struct {
	dns       *dns.Service
	gke       *container.Service
	compute   *compute.Service
	storage   *storage.Service
	functions *cloudfunctions.Service
	run       *run.APIService
	services  schema.ServiceMap
	id        string
	projects  []string
}

var Services = []string{"dns", "gke", "compute", "s3", "cloud-function", "cloud-run"}

const serviceAccountJSON = "gcp_service_account_key"
const providerName = "gcp"

// Name returns the name of the provider
func (p *Provider) Name() string {
	return providerName
}

// ID returns the name of the provider id
func (p *Provider) ID() string {
	return p.id
}

// Services returns the provider services
func (p *Provider) Services() []string {
	return p.services.Keys()
}

// New creates a new provider client for gcp API
func New(options schema.OptionBlock) (*Provider, error) {
	JSONData, ok := options.GetMetadata(serviceAccountJSON)
	if !ok {
		return nil, errorutil.New("could not get API Key")
	}
	id, _ := options.GetMetadata("id")

	provider := &Provider{id: id}
	supportedServicesMap := make(map[string]struct{})
	for _, s := range Services {
		supportedServicesMap[s] = struct{}{}
	}
	services := make(schema.ServiceMap)
	if ss, ok := options.GetMetadata("services"); ok {
		for _, s := range strings.Split(ss, ",") {
			if _, ok := supportedServicesMap[s]; ok {
				services[s] = struct{}{}
			}
		}
	}
	if len(services) == 0 {
		for _, s := range Services {
			services[s] = struct{}{}
		}
	}
	provider.services = services

	creds, err := register(context.Background(), []byte(JSONData))
	if err != nil {
		return nil, errorutil.NewWithErr(err).Msgf("could not register gcp service account")
	}
	if services.Has("dns") {
		dnsService, err := dns.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create dns service with api key")
		}
		provider.dns = dnsService
	}
	if services.Has("compute") {
		computeService, err := compute.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create compute service with api key")
		}
		provider.compute = computeService
	}

	if services.Has("gke") {
		containerService, err := container.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create container service with api key")
		}
		provider.gke = containerService
	}

	if services.Has("s3") {
		storageService, err := storage.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create storage service with api key")
		}
		provider.storage = storageService
	}
	if services.Has("cloud-function") {
		functionsService, err := cloudfunctions.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create functions service with api key")
		}
		provider.functions = functionsService
	}

	if services.Has("cloud-run") {
		cloudRunService, err := run.NewService(context.Background(), creds)
		if err != nil {
			return nil, errorutil.NewWithErr(err).Msgf("could not create cloud run service with api key")
		}
		provider.run = cloudRunService
	}

	projects := []string{}
	manager, err := cloudresourcemanager.NewService(context.Background(), creds)
	if err != nil {
		return nil, errorutil.NewWithErr(err).Msgf("could not list projects")
	}
	list := manager.Projects.List()
	err = list.Pages(context.Background(), func(resp *cloudresourcemanager.ListProjectsResponse) error {
		for _, project := range resp.Projects {
			projects = append(projects, project.ProjectId)
		}
		return nil
	})
	provider.projects = projects
	return provider, err
}

// Resources returns the provider for an resource deployment source.
func (p *Provider) Resources(ctx context.Context) (*schema.Resources, error) {
	finalResources := schema.NewResources()

	if p.dns != nil {
		cloudDNSProvider := &cloudDNSProvider{dns: p.dns, id: p.id, projects: p.projects}
		zones, err := cloudDNSProvider.GetResource(ctx)
		if err != nil {
			return nil, err
		}
		finalResources.Merge(zones)
	}

	if p.gke != nil {
		GKEProvider := &gkeProvider{svc: p.gke, id: p.id, projects: p.projects}
		gkeData, err := GKEProvider.GetResource(ctx)
		if err != nil {
			gologger.Warning().Msgf("Could not get GKE resources: %s\n", err)
		}
		finalResources.Merge(gkeData)
	}

	if p.compute != nil {
		VMProvider := &cloudVMProvider{compute: p.compute, id: p.id, projects: p.projects}
		vmData, err := VMProvider.GetResource(ctx)
		if err != nil {
			return nil, err
		}
		finalResources.Merge(vmData)
	}

	if p.storage != nil {
		cloudStorageProvider := &cloudStorageProvider{id: p.id, storage: p.storage, projects: p.projects}
		storageData, err := cloudStorageProvider.GetResource(ctx)
		if err != nil {
			return nil, err
		}
		finalResources.Merge(storageData)
	}

	if p.functions != nil {
		cloudFunctionsProvider := &cloudFunctionsProvider{id: p.id, functions: p.functions, projects: p.projects}
		functionsData, err := cloudFunctionsProvider.GetResource(ctx)
		if err != nil {
			return nil, err
		}
		finalResources.Merge(functionsData)
	}

	if p.run != nil {
		cloudRunProvider := &cloudRunProvider{id: p.id, run: p.run, projects: p.projects}
		cloudRunData, err := cloudRunProvider.GetResource(ctx)
		if err != nil {
			return nil, err
		}
		finalResources.Merge(cloudRunData)
	}

	return finalResources, nil
}

// Verify checks if the GCP provider credentials are valid
func (p *Provider) Verify(ctx context.Context) error {
	if len(p.projects) == 0 {
		return errorutil.New("no accessible GCP projects found with provided credentials")
	}

	// For extra verification, try a minimal API call on one service
	for _, project := range p.projects {
		var success bool
		if p.compute != nil {
			_, err := p.compute.Regions.List(project).Do()
			if err != nil {
				return errorutil.NewWithErr(err).Msgf("failed to verify compute service access")
			}
			success = true
		} else if p.dns != nil {
			_, err := p.dns.ManagedZones.List(project).Do()
			if err != nil {
				return errorutil.NewWithErr(err).Msgf("failed to verify DNS service access")
			}
			success = true
		} else if p.storage != nil {
			_, err := p.storage.Buckets.List(project).Do()
			if err != nil {
				return errorutil.NewWithErr(err).Msgf("failed to verify storage service access")
			}
			success = true
		} else if p.functions != nil {
			_, err := p.functions.Projects.Locations.List(project).Do()
			if err != nil {
				return errorutil.NewWithErr(err).Msgf("failed to verify functions service access")
			}
			success = true
		} else if p.run != nil {
			_, err := p.run.Projects.Locations.List(project).Do()
			if err != nil {
				return errorutil.NewWithErr(err).Msgf("failed to verify run service access")
			}
			success = true
		}
		// For any one service to be successful, we can return nil
		if success {
			return nil
		}
	}
	return errorutil.New("no accessible GCP services found with provided credentials")
}
