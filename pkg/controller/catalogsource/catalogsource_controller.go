package catalogsource

import (
	"context"
	"time"

	olm "github.com/operator-framework/operator-marketplace/pkg/apis/olm/v1alpha1"
	"github.com/operator-framework/operator-marketplace/pkg/controller/options"
	"github.com/operator-framework/operator-marketplace/pkg/defaults"
	"github.com/operator-framework/operator-marketplace/pkg/operatorhub"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Add creates a new CatalogSource Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, _ options.ControllerOptions) error {
	return add(mgr, newReconciler(mgr))
}

func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	client := mgr.GetClient()
	return &ReconcileCatalogSource{
		client: client,
	}
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {

	c, err := controller.New("catalogsource-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	_, defaultCatalogsources := defaults.GetGlobalDefinitions()
	pred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if _, ok := defaultCatalogsources[e.MetaOld.GetName()]; ok {
				return true
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			if _, ok := defaultCatalogsources[e.Meta.GetName()]; ok {
				// If DeleteStateUnknown is true it implies that the Delete event was missed
				// and we can ignore it.
				if e.DeleteStateUnknown {
					return false
				}
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			if _, ok := defaultCatalogsources[e.Meta.GetName()]; ok {
				return true
			}
			return false
		},
	}

	err = c.Watch(&source.Kind{Type: &olm.CatalogSource{}}, &handler.EnqueueRequestForObject{}, pred)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileOperatorHub implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileCatalogSource{}

// ReconcileCatalogSource reconciles a CatalogSource object
type ReconcileCatalogSource struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
}

func (r *ReconcileCatalogSource) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Infof("Reconciling default CatalogSource %s", request.Name)

	_, defaultCatalogsources := defaults.GetGlobalDefinitions()
	defaultCatsrcDef := defaultCatalogsources[request.Name]
	// Fetch the CatalogSource instance
	instance := &olm.CatalogSource{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			if !operatorhub.GetSingleton().Get()[defaultCatsrcDef.Name] {
				createNewCatsrcInstance(r.client, defaultCatsrcDef)
			}
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 5}, err
	}

	if instance.DeletionTimestamp != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 5}, nil
	}

	if !defaults.AreCatsrcSpecsEqual(&defaultCatsrcDef.Spec, &instance.Spec) {
		if err := r.client.Delete(context.TODO(), instance); err != nil {
			log.Warnf("Could not set default CatalogSource %s's spec back to desired default state. Error in deleting updated CatalogSource: %s", defaultCatsrcDef.GetName(), err.Error())
		}
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 5}, nil
	}
	return reconcile.Result{}, nil
}

func createNewCatsrcInstance(client client.Client, catsrc olm.CatalogSource) error {
	err := client.Create(context.TODO(), &catsrc)
	if err != nil {
		log.Warnf("Could not recreate default CatalogSource %s. Error: %s", catsrc.GetName(), err.Error())
		return err
	}
	return nil
}
