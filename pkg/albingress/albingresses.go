package albingress

import (
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"

	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/tools/record"
	"k8s.io/ingress/core/pkg/ingress/annotations/class"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/annotations"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/aws/albec2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/aws/albelbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/aws/albwaf"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
)

// NewALBIngressesFromIngressesOptions are the options to NewALBIngressesFromIngresses
type NewALBIngressesFromIngressesOptions struct {
	Recorder              record.EventRecorder
	ClusterName           string
	ALBNamePrefix         string
	Ingresses             []interface{}
	ALBIngresses          ALBIngresses
	IngressClass          string
	DefaultIngressClass   string
	GetServiceNodePort    func(string, int32) (*int64, error)
	GetServiceAnnotations func(string, string) (*map[string]string, error)
	GetNodes              func() util.AWSStringSlice
	AnnotationFactory     annotations.AnnotationFactory
}

// NewALBIngressesFromIngresses returns a ALBIngresses created from the Kubernetes ingress state.
func NewALBIngressesFromIngresses(o *NewALBIngressesFromIngressesOptions) ALBIngresses {
	var ALBIngresses ALBIngresses

	// Find every ingress currently in Kubernetes.
	for _, k8singress := range o.Ingresses {
		ingResource := k8singress.(*extensions.Ingress)

		// Ensure the ingress resource found contains an appropriate ingress class.
		if !class.IsValid(ingResource, o.IngressClass, o.DefaultIngressClass) {
			continue
		}

		// Find the existing ingress for this Kubernetes ingress (if it existed).
		id := GenerateID(ingResource.GetNamespace(), ingResource.Name)
		_, existingIngress := o.ALBIngresses.FindByID(id)

		// Produce a new ALBIngress instance for every ingress found. If ALBIngress returns nil, there
		// was an issue with the ingress (e.g. bad annotations) and should not be added to the list.
		ALBIngress := NewALBIngressFromIngress(&NewALBIngressFromIngressOptions{
			Ingress:               ingResource,
			ExistingIngress:       existingIngress,
			ClusterName:           o.ClusterName,
			ALBNamePrefix:         o.ALBNamePrefix,
			GetServiceNodePort:    o.GetServiceNodePort,
			GetServiceAnnotations: o.GetServiceAnnotations,
			GetNodes:              o.GetNodes,
			Recorder:              o.Recorder,
			AnnotationFactory:     o.AnnotationFactory,
		})

		// Add the new ALBIngress instance to the new ALBIngress list.
		ALBIngresses = append(ALBIngresses, ALBIngress)
	}
	return ALBIngresses
}

// AssembleIngressesFromAWSOptions are the options to AssembleIngressesFromAWS
type AssembleIngressesFromAWSOptions struct {
	Recorder      record.EventRecorder
	ALBNamePrefix string
}

// AssembleIngressesFromAWS builds a list of existing ingresses from resources in AWS
func AssembleIngressesFromAWS(o *AssembleIngressesFromAWSOptions) ALBIngresses {
	var ingresses ALBIngresses
	var wg sync.WaitGroup

	logger.Infof("Building list of existing ALBs")
	t0 := time.Now()

	// Fetch a list of load balancers that match this cluser name
	loadBalancers, err := albelbv2.ELBV2svc.ClusterLoadBalancers(&o.ALBNamePrefix)
	if err != nil {
		logger.Fatalf(err.Error())
	}

	logger.Infof("Fetching information on %d ALBs", len(loadBalancers))

	// Generate the list of ingresses from those load balancers
	for _, loadBalancer := range loadBalancers {
		wg.Add(1)
		go func(wg *sync.WaitGroup, loadBalancer *elbv2.LoadBalancer) {
			defer wg.Done()

			var managedSG *string
			var managedInstanceSG *string
			managedSGInboundCidrs := []*string{}
			managedSGPorts := []int64{}
			if len(loadBalancer.SecurityGroups) == 1 {
				tags, err := albec2.EC2svc.DescribeSGTags(loadBalancer.SecurityGroups[0])
				if err != nil {
					logger.Fatalf(err.Error())
				}

				for _, tag := range tags {
					// If the subnet is labeled as managed by ALB, capture it as the managedSG
					if *tag.Key == albec2.ManagedByKey && *tag.Value == albec2.ManagedByValue {
						managedSG = loadBalancer.SecurityGroups[0]
						ports, err := albec2.EC2svc.DescribeSGPorts(loadBalancer.SecurityGroups[0])
						if err != nil {
							logger.Fatalf("Failed to describe ports of managed security group. Error: %s", err.Error())
						}

						managedSGPorts = ports

						cidrs, err := albec2.EC2svc.DescribeSGInboundCidrs(loadBalancer.SecurityGroups[0])
						if err != nil {
							logger.Fatalf("Failed to describe ingress ipv4 ranges of managed security group. Error: %s", err.Error())
						}
						managedSGInboundCidrs = cidrs
					}
				}
				// when a alb-managed SG existed, we must find a correlated instance SG
				if managedSG != nil {
					instanceSG, err := albec2.EC2svc.DescribeSGByPermissionGroup(managedSG)
					if err != nil {
						logger.Fatalf("Failed to find related managed instance SG. Was it deleted from AWS? Error: %s", err.Error())
					}
					managedInstanceSG = instanceSG
				}
			}

			var idleTimeout *int64
			in := &elbv2.DescribeLoadBalancerAttributesInput{
				LoadBalancerArn: loadBalancer.LoadBalancerArn,
			}
			attrs, err := albelbv2.ELBV2svc.DescribeLoadBalancerAttributes(in)
			if err != nil {
				logger.Fatalf("Failed to retrieve attributes from ALB in AWS. Error: %s", err.Error())
			}
			for _, attr := range attrs.Attributes {
				if *attr.Key == util.IdleTimeoutKey {
					idleTimeoutInt64, err := strconv.ParseInt(*attr.Value, 10, 64)
					if err != nil {
						logger.Fatalf("Failed to parse idle timeout value from ALB attribute. Was: %s", *attr.Value)
					}
					idleTimeout = aws.Int64(idleTimeoutInt64)
				}
			}

			// Check WAF
			wafResult, err := albwaf.WAFRegionalsvc.GetWebACLSummary(loadBalancer.LoadBalancerArn)
			if err != nil {
				logger.Fatalf("Failed to get associated WAF ACL. Error: %s", err.Error())
			}
			var wafACLID *string
			if wafResult != nil {
				wafACLID = wafResult.WebACLId
			}

			albIngress, err := NewALBIngressFromAWSLoadBalancer(&NewALBIngressFromAWSLoadBalancerOptions{
				LoadBalancer:          loadBalancer,
				ALBNamePrefix:         o.ALBNamePrefix,
				Recorder:              o.Recorder,
				ManagedSG:             managedSG,
				ManagedSGInboundCidrs: managedSGInboundCidrs,
				ManagedSGPorts:        managedSGPorts,
				ManagedInstanceSG:     managedInstanceSG,
				ConnectionIdleTimeout: idleTimeout,
				WafACLID:              wafACLID,
			})
			if err != nil {
				logger.Infof(err.Error())
			} else {
				ingresses = append(ingresses, albIngress)
			}

		}(&wg, loadBalancer)
	}
	wg.Wait()

	logger.Infof("Assembled %d ingresses from existing AWS resources in %v", len(ingresses), time.Now().Sub(t0))
	return ingresses
}

// FindByID locates the ingress by the id parameter and returns its position
func (a ALBIngresses) FindByID(id string) (int, *ALBIngress) {
	for p, v := range a {
		if v.id == id {
			return p, v
		}
	}
	return -1, nil
}

// RemovedIngresses compares the ingress list to the ingress list in the type, returning any ingresses that
// are not in the ingress list parameter.
func (a ALBIngresses) RemovedIngresses(newList ALBIngresses) ALBIngresses {
	var deleteableIngress ALBIngresses

	// Loop through every ingress in current (old) ingress list known to ALBController
	for _, ingress := range a {
		// Ingress objects not found in newList might qualify for deletion.
		if i, _ := newList.FindByID(ingress.id); i < 0 {
			// If the ALBIngress still contains a LoadBalancer, it still needs to be deleted.
			// In this case, strip all desired state and add it to the deleteableIngress list.
			// If the ALBIngress contains no LoadBalancer, it was previously deleted and is
			// no longer relevant to the ALBController.
			if ingress.loadBalancer != nil {
				ingress.stripDesiredState()
				deleteableIngress = append(deleteableIngress, ingress)
			}
		}
	}
	return deleteableIngress
}