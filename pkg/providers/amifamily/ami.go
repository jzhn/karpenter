/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package amifamily

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/aws-sdk-go/service/ssm/ssmiface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/v1beta1"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/pretty"
)

type Provider struct {
	cache                  *cache.Cache
	kubernetesVersionCache *cache.Cache
	ssm                    ssmiface.SSMAPI
	kubeClient             client.Client
	ec2api                 ec2iface.EC2API
	cm                     *pretty.ChangeMonitor
	kubernetesInterface    kubernetes.Interface
}

type AMI struct {
	Name         string
	AmiID        string
	CreationDate string
	Requirements scheduling.Requirements
}

type AMIs []AMI

// Sort orders the AMIs by creation date in descending order.
// If creation date is nil or two AMIs have the same creation date, the AMIs will be sorted by name in ascending order.
func (a AMIs) Sort() {
	sort.Slice(a, func(i, j int) bool {
		if a[i].CreationDate != "" || a[j].CreationDate != "" {
			itime, _ := time.Parse(time.RFC3339, a[i].CreationDate)
			jtime, _ := time.Parse(time.RFC3339, a[j].CreationDate)
			if itime.Unix() != jtime.Unix() {
				return itime.Unix() >= jtime.Unix()
			}
		}
		return a[i].Name >= a[j].Name
	})
}

func (a AMIs) String() string {
	var sb strings.Builder
	ids := lo.Map(a, func(a AMI, _ int) string { return a.AmiID })
	if len(a) > 25 {
		sb.WriteString(strings.Join(ids[:25], ", "))
		sb.WriteString(fmt.Sprintf(" and %d other(s)", len(a)-25))
	} else {
		sb.WriteString(strings.Join(ids, ", "))
	}
	return sb.String()
}

// MapToInstanceTypes returns a map of AMIIDs that are the most recent on creationDate to compatible instancetypes
func (a AMIs) MapToInstanceTypes(instanceTypes []*cloudprovider.InstanceType) map[string][]*cloudprovider.InstanceType {
	amiIDs := map[string][]*cloudprovider.InstanceType{}
	for _, instanceType := range instanceTypes {
		for _, ami := range a {
			if err := instanceType.Requirements.Compatible(ami.Requirements); err == nil {
				amiIDs[ami.AmiID] = append(amiIDs[ami.AmiID], instanceType)
				break
			}
		}
	}
	return amiIDs
}

const (
	kubernetesVersionCacheKey = "kubernetesVersion"
)

func NewProvider(kubeClient client.Client, kubernetesInterface kubernetes.Interface, ssm ssmiface.SSMAPI, ec2api ec2iface.EC2API,
	cache, kubernetesVersionCache *cache.Cache) *Provider {
	return &Provider{
		cache:                  cache,
		kubernetesVersionCache: kubernetesVersionCache,
		ssm:                    ssm,
		kubeClient:             kubeClient,
		ec2api:                 ec2api,
		cm:                     pretty.NewChangeMonitor(),
		kubernetesInterface:    kubernetesInterface,
	}
}

func (p *Provider) KubeServerVersion(ctx context.Context) (string, error) {
	if version, ok := p.kubernetesVersionCache.Get(kubernetesVersionCacheKey); ok {
		return version.(string), nil
	}
	serverVersion, err := p.kubernetesInterface.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	version := fmt.Sprintf("%s.%s", serverVersion.Major, strings.TrimSuffix(serverVersion.Minor, "+"))
	p.kubernetesVersionCache.SetDefault(kubernetesVersionCacheKey, version)
	if p.cm.HasChanged("kubernetes-version", version) {
		logging.FromContext(ctx).With("version", version).Debugf("discovered kubernetes version")
	}
	return version, nil
}

// Get Returning a list of AMIs with its associated requirements
func (p *Provider) Get(ctx context.Context, nodeClass *v1beta1.NodeClass, options *Options) (AMIs, error) {
	var err error
	var amis AMIs
	if len(nodeClass.Spec.AMISelectorTerms) == 0 {
		amis, err = p.getDefaultAMIs(ctx, nodeClass, options)
		if err != nil {
			return nil, err
		}
	} else {
		amis, err = p.getAMIs(ctx, nodeClass.Spec.AMISelectorTerms)
		if err != nil {
			return nil, err
		}
	}
	amis.Sort()
	if p.cm.HasChanged(fmt.Sprintf("amis/%t/%s", nodeClass.IsNodeTemplate, nodeClass.Name), amis) {
		logging.FromContext(ctx).With("ids", amis, "count", len(amis)).Debugf("discovered amis")
	}
	return amis, nil
}

func (p *Provider) getDefaultAMIs(ctx context.Context, nodeClass *v1beta1.NodeClass, options *Options) (res AMIs, err error) {
	if images, ok := p.cache.Get(lo.FromPtr(nodeClass.Spec.AMIFamily)); ok {
		return images.(AMIs), nil
	}
	amiFamily := GetAMIFamily(nodeClass.Spec.AMIFamily, options)
	kubernetesVersion, err := p.KubeServerVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting kubernetes version %w", err)
	}
	defaultAMIs := amiFamily.DefaultAMIs(kubernetesVersion)
	for _, ami := range defaultAMIs {
		if id, err := p.resolveSSMParameter(ctx, ami.Query); err != nil {
			logging.FromContext(ctx).With("query", ami.Query).Errorf("discovering amis from ssm, %s", err)
		} else {
			res = append(res, AMI{AmiID: id, Requirements: ami.Requirements})
		}
	}
	// Resolve Name and CreationDate information into the DefaultAMIs
	if err = p.ec2api.DescribeImagesPagesWithContext(ctx, &ec2.DescribeImagesInput{
		Filters:    []*ec2.Filter{{Name: aws.String("image-id"), Values: aws.StringSlice(lo.Map(res, func(a AMI, _ int) string { return a.AmiID }))}},
		MaxResults: aws.Int64(500),
	}, func(page *ec2.DescribeImagesOutput, _ bool) bool {
		for i := range page.Images {
			for j := range res {
				if res[j].AmiID == aws.StringValue(page.Images[i].ImageId) {
					res[j].Name = aws.StringValue(page.Images[i].Name)
					res[j].CreationDate = aws.StringValue(page.Images[i].CreationDate)
				}
			}
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("describing images, %w", err)
	}
	p.cache.SetDefault(lo.FromPtr(nodeClass.Spec.AMIFamily), res)
	return res, nil
}

func (p *Provider) resolveSSMParameter(ctx context.Context, ssmQuery string) (string, error) {
	output, err := p.ssm.GetParameterWithContext(ctx, &ssm.GetParameterInput{Name: aws.String(ssmQuery)})
	if err != nil {
		return "", fmt.Errorf("getting ssm parameter %q, %w", ssmQuery, err)
	}
	ami := aws.StringValue(output.Parameter.Value)
	return ami, nil
}

func (p *Provider) getAMIs(ctx context.Context, terms []v1beta1.AMISelectorTerm) (AMIs, error) {
	filterAndOwnerSets := GetFilterAndOwnerSets(terms)
	hash, err := hashstructure.Hash(filterAndOwnerSets, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	if err != nil {
		return nil, err
	}
	if images, ok := p.cache.Get(fmt.Sprint(hash)); ok {
		return images.(AMIs), nil
	}
	images := map[uint64]AMI{}
	for _, filtersAndOwners := range filterAndOwnerSets {
		if err = p.ec2api.DescribeImagesPagesWithContext(ctx, &ec2.DescribeImagesInput{
			// Don't include filters in the Describe Images call as EC2 API doesn't allow empty filters.
			Filters:    lo.Ternary(len(filtersAndOwners.Filters) > 0, filtersAndOwners.Filters, nil),
			Owners:     lo.Ternary(len(filtersAndOwners.Owners) > 0, aws.StringSlice(filtersAndOwners.Owners), nil),
			MaxResults: aws.Int64(500),
		}, func(page *ec2.DescribeImagesOutput, _ bool) bool {
			for i := range page.Images {
				reqs := p.getRequirementsFromImage(page.Images[i])
				if !v1beta1.WellKnownArchitectures.Has(reqs.Get(v1.LabelArchStable).Any()) {
					continue
				}
				reqsHash := lo.Must(hashstructure.Hash(reqs.NodeSelectorRequirements(), hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true}))
				// If the proposed image is newer, store it so that we can return it
				if v, ok := images[reqsHash]; ok {
					candidateCreationTime, _ := time.Parse(time.RFC3339, lo.FromPtr(page.Images[i].CreationDate))
					existingCreationTime, _ := time.Parse(time.RFC3339, v.CreationDate)
					if existingCreationTime == candidateCreationTime && lo.FromPtr(page.Images[i].Name) < v.Name {
						continue
					}
					if candidateCreationTime.Unix() < existingCreationTime.Unix() {
						continue
					}
				}
				images[reqsHash] = AMI{
					Name:         lo.FromPtr(page.Images[i].Name),
					AmiID:        lo.FromPtr(page.Images[i].ImageId),
					CreationDate: lo.FromPtr(page.Images[i].CreationDate),
					Requirements: reqs,
				}
			}
			return true
		}); err != nil {
			return nil, fmt.Errorf("describing images, %w", err)
		}
	}
	p.cache.SetDefault(fmt.Sprint(hash), AMIs(lo.Values(images)))
	return lo.Values(images), nil
}

type FiltersAndOwners struct {
	Filters []*ec2.Filter
	Owners  []string
}

func GetFilterAndOwnerSets(terms []v1beta1.AMISelectorTerm) (res []FiltersAndOwners) {
	idFilter := &ec2.Filter{Name: aws.String("image-id")}
	for _, term := range terms {
		switch {
		case term.ID != "":
			idFilter.Values = append(idFilter.Values, aws.String(term.ID))
		default:
			elem := FiltersAndOwners{
				Owners: lo.Ternary(term.Owner != "", []string{term.Owner}, []string{"self", "amazon"}),
			}
			if term.Name != "" {
				elem.Filters = append(elem.Filters, &ec2.Filter{
					Name:   aws.String("name"),
					Values: aws.StringSlice([]string{term.Name}),
				})
			}
			for k, v := range term.Tags {
				if v == "*" {
					elem.Filters = append(elem.Filters, &ec2.Filter{
						Name:   aws.String("tag-key"),
						Values: []*string{aws.String(k)},
					})
				} else {
					elem.Filters = append(elem.Filters, &ec2.Filter{
						Name:   aws.String(fmt.Sprintf("tag:%s", k)),
						Values: aws.StringSlice(functional.SplitCommaSeparatedString(v)),
					})
				}
			}
			res = append(res, elem)
		}
	}
	if len(idFilter.Values) > 0 {
		res = append(res, FiltersAndOwners{Filters: []*ec2.Filter{idFilter}})
	}
	return res
}

func (p *Provider) getRequirementsFromImage(ec2Image *ec2.Image) scheduling.Requirements {
	requirements := scheduling.NewRequirements()
	for _, tag := range ec2Image.Tags {
		if v1alpha5.WellKnownLabels.Has(*tag.Key) {
			requirements.Add(scheduling.NewRequirement(*tag.Key, v1.NodeSelectorOpIn, *tag.Value))
		}
	}
	// Always add the architecture of an image as a requirement, irrespective of what's specified in EC2 tags.
	architecture := *ec2Image.Architecture
	if value, ok := v1alpha1.AWSToKubeArchitectures[architecture]; ok {
		architecture = value
	}
	requirements.Add(scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, architecture))
	return requirements
}
