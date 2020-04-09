package admiral

import (
	"fmt"
	argo "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	argoclientset "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned"
	argoinformers "github.com/argoproj/argo-rollouts/pkg/client/informers/externalversions"
	argoprojv1alpha1 "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned/typed/rollouts/v1alpha1"
	"github.com/istio-ecosystem/admiral/admiral/pkg/controller/common"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/kubernetes"
	"sync"
	"time"
)

// Handler interface contains the methods that are required
type RolloutHandler interface {
	Added(obj *argo.Rollout)
	Updated(obj *argo.Rollout)
	Deleted(obj *argo.Rollout)
}

type RolloutsEntry struct {
	Identity string
	Rollout *argo.Rollout
}

type RolloutClusterEntry struct {
	Identity    string
	Rollouts map[string][]*argo.Rollout
}

type RolloutController struct {
	K8sClient         kubernetes.Interface
	RolloutClient     argoprojv1alpha1.ArgoprojV1alpha1Interface
	RolloutHandler RolloutHandler
	informer  cache.SharedIndexInformer
	ctl      *Controller
	clusterName string
	Cache    *rolloutCache
	labelSet 		  *common.LabelSet
}


type rolloutCache struct {
	//map of dependencies key=identity value array of onboarded identities
	cache map[string]*RolloutClusterEntry
	mutex *sync.Mutex
}

func (p *rolloutCache) Put(rolloutEntry *RolloutClusterEntry) {
	defer p.mutex.Unlock()
	p.mutex.Lock()

	p.cache[rolloutEntry.Identity] = rolloutEntry
}

func (p *rolloutCache) getKey(rollout *argo.Rollout) string {
	return common.GetRolloutGlobalIdentifier(rollout)
}

func (p *rolloutCache) Get(key string) *RolloutClusterEntry {
	return p.cache[key]
}

func (p *rolloutCache) Delete(pod *RolloutClusterEntry) {
	defer p.mutex.Unlock()
	p.mutex.Lock()
	delete(p.cache, pod.Identity)
}

func (p *rolloutCache) AppendRolloutToCluster(key string, rollout *argo.Rollout) {
	defer p.mutex.Unlock()
	p.mutex.Lock()

	v := p.Get(key)

	if v == nil {
		v = &RolloutClusterEntry{
			Identity:    key,
			Rollouts: make(map[string][]*argo.Rollout),
		}
		p.cache[v.Identity] = v
	}
	env := common.GetEnvForRollout(rollout)
	envRollouts := v.Rollouts[env]

	if envRollouts == nil {
		envRollouts = make([]*argo.Rollout, 0)
	}

	envRollouts = append(envRollouts, rollout)

	v.Rollouts[env] = envRollouts

}

func (d *RolloutController) GetRollouts() ([]*argo.Rollout, error) {

	ns := d.K8sClient.CoreV1().Namespaces()

	namespaceSidecarInjectionLabelFilter := d.labelSet.NamespaceSidecarInjectionLabel+"="+d.labelSet.NamespaceSidecarInjectionLabelValue
	istioEnabledNs, err := ns.List(meta_v1.ListOptions{LabelSelector: namespaceSidecarInjectionLabelFilter})

	if err != nil {
		return nil, fmt.Errorf("error getting istio labled namespaces: %v", err)
	}

	var res []*argo.Rollout

	for _, v := range istioEnabledNs.Items {

		//rollouts := d.K8sClient.AppsV1().Deployments(v.Name)
		rollouts := d.RolloutClient.Rollouts(v.Name)
		rolloutsList, err := rollouts.List(meta_v1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing rollouts: %v", err)
		}
		var admiralRollouts []argo.Rollout
		for _, rollout := range rolloutsList.Items {
			if !d.shouldIgnoreBasedOnLabelsForRollout(&rollout) {
				admiralRollouts = append(admiralRollouts, rollout)
			}
		}

		if err != nil {
			return nil, fmt.Errorf("error getting istio labled namespaces: %v", err)
		}

		for _, pi := range admiralRollouts {
			res = append(res, &pi)
		}
	}

	return res, nil
}

func (d *RolloutController) shouldIgnoreBasedOnLabelsForRollout(rollout *argo.Rollout) bool {
	if rollout.Spec.Template.Labels[d.labelSet.AdmiralIgnoreLabel] == "true" { //if we should ignore, do that and who cares what else is there
		return true
	}
	if rollout.Spec.Template.Annotations[d.labelSet.DeploymentAnnotation] != "true" { //Not sidecar injected, we don't want to inject
		return true
	}
	return false //labels are fine, we should not ignore
}


func NewRolloutsController(stopCh <-chan struct{}, handler RolloutHandler, config *rest.Config, resyncPeriod time.Duration) (*RolloutController, error) {

	roController := RolloutController{}
	roController.RolloutHandler = handler
	roController.labelSet = common.GetLabelSet()

	rolloutCache := rolloutCache{}
	rolloutCache.cache = make(map[string]*RolloutClusterEntry)
	rolloutCache.mutex = &sync.Mutex{}

	roController.Cache = &rolloutCache

	var err error
	rolloutClient, err := argoclientset.NewForConfig(config)


	if err != nil {
		return nil, fmt.Errorf("failed to create rollouts controller argo client: %v", err)
	}

	roController.K8sClient, err = K8sClientFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create rollouts controller k8s client: %v", err)
	}

	roController.RolloutClient = rolloutClient.ArgoprojV1alpha1()

	argoRolloutsInformerFactory := argoinformers.NewSharedInformerFactoryWithOptions(
		rolloutClient,
		resyncPeriod,
		argoinformers.WithNamespace(meta_v1.NamespaceAll))
	//Initialize informer
	roController.informer = argoRolloutsInformerFactory.Argoproj().V1alpha1().Rollouts().Informer()

	NewController(stopCh, &roController, roController.informer)
	return &roController, nil
}

func (roc *RolloutController) Added(ojb interface{}) {

	rollout := ojb.(*argo.Rollout)
	key := roc.Cache.getKey(rollout)
	if len(key) > 0 && !roc.shouldIgnoreBasedOnLabelsForRollout(rollout) {
		roc.Cache.AppendRolloutToCluster(key, rollout)
		roc.RolloutHandler.Added(rollout)
	}
}

func (roc *RolloutController) Updated(ojb interface{}) {
	rollout := ojb.(*argo.Rollout)
	key := roc.Cache.getKey(rollout)
	if len(key) > 0 && !roc.shouldIgnoreBasedOnLabelsForRollout(rollout) {
		roc.Cache.AppendRolloutToCluster(key, rollout)
		roc.RolloutHandler.Added(rollout)
	}
}

func (sec *RolloutController) Deleted(ojb interface{}) {
	//TODO deal with this

}
