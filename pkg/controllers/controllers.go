package controllers

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scaleway/scaleway-sdk-go/scw"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

const (
	ReverseIPDomainEnv = "REVERSE_IP_DOMAIN"
	DatabaseIDsEnv     = "DATABASE_IDS"
	ReservedIPsPoolEnv = "RESERVED_IPS_POOL"
)

func NewController(clientset *kubernetes.Clientset) (*Controller, error) {
	nodeListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "nodes", "", fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(nodeListWatcher, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {

				oldNode, oldOk := old.(*v1.Node)
				newNode, newOk := new.(*v1.Node)
				if oldOk && newOk {
					if oldNode.ResourceVersion == newNode.ResourceVersion {
						queue.Add(key)
						return
					}
					for _, oldAddress := range oldNode.Status.Addresses {
						for _, newAddress := range newNode.Status.Addresses {
							if oldAddress.Type == newAddress.Type && oldAddress.Address != newAddress.Address {
								queue.Add(key)
								return
							}
						}
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	scwClient, err := scw.NewClient(scw.WithEnv())
	if err != nil {
		return nil, err
	}

	controller := &Controller{
		indexer:   indexer,
		informer:  informer,
		queue:     queue,
		scwClient: scwClient,
	}

	// TODO handle validation here ?
	if os.Getenv(ReverseIPDomainEnv) != "" {
		controller.reverseIPDomain = os.Getenv(ReverseIPDomainEnv)
	}

	if os.Getenv(DatabaseIDsEnv) != "" {
		controller.databaseIDs = strings.Split(os.Getenv(DatabaseIDsEnv), ",")
	}

	if os.Getenv(ReservedIPsPoolEnv) != "" {
		controller.reservedIPs = strings.Split(os.Getenv(ReservedIPsPoolEnv), ",")
	}

	return controller, nil
}

func (c *Controller) syncNeeded(nodeName string) error {
	var errs []error

	err := c.syncReservedIP(nodeName)
	if err != nil {
		klog.Errorf("failed to sync reserved IP for node %s: %v", nodeName, err)
		errs = append(errs, err)
	}
	err = c.syncReverseIP(nodeName)
	if err != nil {
		klog.Errorf("failed to sync reverse IP for node %s: %v", nodeName, err)
		errs = append(errs, err)
	}
	err = c.syncDatabaseACLs(nodeName)
	if err != nil {
		klog.Errorf("failed to sync database acl for node %s: %v", nodeName, err)
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("got several error")
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncNeeded(key.(string))
	c.handleErr(err, key)
	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < c.numberRetries {
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	runtime.HandleError(err)
	klog.Infof("too many retries for key %s: %v", key, err)
}

func (c *Controller) Run(stopCh chan struct{}) {
	defer runtime.HandleCrash()
	defer c.Wg.Done()

	defer c.queue.ShutDown()

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}
