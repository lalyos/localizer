// Copyright 2020 Jared Allard
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package proxier

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/metal-stack/go-ipam"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/txn2/txeh"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

type worker struct {
	k    kubernetes.Interface
	rest *rest.Config
	log  logrus.FieldLogger

	ippool ipam.Ipamer
	ipCidr string
	dns    *txeh.Hosts

	reqChan    chan PortForwardRequest
	reaperChan chan *corev1.Endpoints
	doneChan   chan<- struct{}

	// portForwards are existing port-forwards
	portForwards map[string]*PortForwardConnection
}

// NewPortForwarder creates a new port-forward worker that handles
// creating port-forwards and destroying port-forwards.
//nolint:gocritic,lll // We're OK not naming these.
func NewPortForwarder(ctx context.Context, k kubernetes.Interface, r *rest.Config, log logrus.FieldLogger) (chan<- PortForwardRequest, <-chan struct{}, *worker, error) {
	ipamInstance := ipam.New()
	prefix, err := ipamInstance.NewPrefix("127.0.0.1/8")
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to create ip pool")
	}

	// ensure that 127.0.0.1 is never allocated
	_, err = ipamInstance.AcquireSpecificIP(prefix.Cidr, "127.0.0.1")
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to create ip pool")
	}

	hosts, err := txeh.NewHosts(&txeh.HostsConfig{})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to open up hosts file for r/w")
	}

	doneChan := make(chan struct{})
	reqChan := make(chan PortForwardRequest, 1024)
	reaperChan := make(chan *corev1.Endpoints, 1024)

	_, endpointInformer := cache.NewInformer(
		cache.NewListWatchFromClient(k.CoreV1().RESTClient(), "endpoints", corev1.NamespaceAll, fields.Everything()),
		&corev1.Endpoints{},
		time.Second*60,
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj interface{}) {
				reaperChan <- obj.(*corev1.Endpoints)
			},
		},
	)

	w := &worker{
		k:            k,
		rest:         r,
		log:          log,
		ippool:       ipamInstance,
		ipCidr:       prefix.Cidr,
		dns:          hosts,
		reqChan:      reqChan,
		reaperChan:   reaperChan,
		doneChan:     doneChan,
		portForwards: make(map[string]*PortForwardConnection),
	}

	go endpointInformer.Run(ctx.Done())
	go w.Reaper(ctx)
	go w.Start(ctx)

	return reqChan, doneChan, w, nil
}

// Repear reaps dead connections based off of endpoint updates
func (w *worker) Reaper(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case endpoints := <-w.reaperChan:
			// check if we care about this endpoint by checking if it's
			// part of our registered services
			conns, ok := w.portForwards[(&ServiceInfo{endpoints.Name, endpoints.Namespace, ""}).Key()]
			if !ok {
				continue
			}

			foundEndpoints := make(map[PodInfo]bool)
			for _, subset := range endpoints.Subsets {
				for _, addr := range subset.Addresses {
					if addr.TargetRef == nil {
						continue
					}

					if addr.TargetRef.Kind != "Pod" {
						continue
					}

					foundEndpoints[PodInfo{addr.TargetRef.Name, addr.TargetRef.Namespace}] = true
				}
			}

			// endpoint still exists, so don't do anything
			if _, ok := foundEndpoints[conns.Pod]; ok {
				continue
			}

			reason := fmt.Sprintf("endpoints '%s' was removed", conns.Pod.Key())

			// handle a service that had no endpoints before, but now does
			// TODO: use ptr
			if conns.Pod.Key() == "/" {
				if len(foundEndpoints) != 0 {
					reason = "found endpoints, service originally had none"
				} else {
					// if no endpoints still, then ignore it
					continue
				}
			}

			// refresh pods we didn't find
			w.reqChan <- PortForwardRequest{
				CreatePortForwardRequest: &CreatePortForwardRequest{
					Service:        conns.Service,
					Hostnames:      conns.Hostnames,
					Ports:          conns.Ports,
					Recreate:       true,
					RecreateReason: reason,
				},
			}
		}
	}
}

// Start starts the worker process. This is done when the worker is created
// and should be run in a goroutine if this is created manually.
func (w *worker) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for info := range w.portForwards {
				err := w.DeletePortForward(ctx, &DeletePortForwardRequest{
					Service: w.portForwards[info].Service,
				})
				if err != nil {
					w.log.WithError(err).Warn("failed to clean up port-forward")
				}
			}

			// close our channel(s)
			close(w.doneChan)

			return
		case req := <-w.reqChan:
			var serv ServiceInfo
			var err error
			if req.CreatePortForwardRequest != nil {
				err = w.CreatePortForward(ctx, req.CreatePortForwardRequest)
				serv = req.CreatePortForwardRequest.Service
			} else if req.DeletePortForwardRequest != nil {
				err = w.DeletePortForward(ctx, req.DeletePortForwardRequest)
				serv = req.DeletePortForwardRequest.Service
			}

			log := w.log.WithField("service", serv.Key())

			if err != nil {
				log.WithError(err).Errorf("encountered an error: %v", err)
			}
		}
	}
}

// getPodForService finds the first available endpoint for a given service
func (w *worker) getPodForService(ctx context.Context, si *ServiceInfo) (PodInfo, error) {
	e, err := w.k.CoreV1().Endpoints(si.Namespace).Get(ctx, si.Name, metav1.GetOptions{})
	if err != nil {
		return PodInfo{}, err
	}

	found := false
	pod := PodInfo{}

loop:
	for _, subset := range e.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef == nil {
				continue
			}

			if addr.TargetRef.Kind != "Pod" {
				continue
			}

			found = true
			pod.Name = addr.TargetRef.Name
			pod.Namespace = addr.TargetRef.Namespace

			break loop
		}
	}
	if !found {
		return pod, fmt.Errorf("failed to find endpoint for service")
	}

	return pod, nil
}

func (w *worker) CreatePortForward(ctx context.Context, req *CreatePortForwardRequest) (returnedError error) {
	serviceKey := req.Service.Key()
	log := w.log.WithField("service", serviceKey)
	if req.Endpoint != nil {
		log = log.WithField("endpoint", req.Endpoint.Key())
	}

	// skip port-forwards that are already being managed
	// unless it's marked as being recreated
	if _, ok := w.portForwards[serviceKey]; ok && !req.Recreate {
		return fmt.Errorf("already have a port-forward for this service")
	}

	if req.Recreate {
		log.Infof("recreating port-forward due to: %v", req.RecreateReason)
		w.setPortForwardConnectionStatus(ctx, req.Service, PortForwardStatusRecreating, req.RecreateReason)
		err := w.stopPortForward(ctx, w.portForwards[serviceKey])
		if err != nil {
			log.WithError(err).Warn("failed to cleanup previous port-forward")
		}
	}

	pf := &PortForwardConnection{
		Service: req.Service,
		Status:  PortForwardStatusRunning,
		Ports:   req.Ports,
	}

	// cleanup after failed tunnel (that failed to be created)
	// using named returns we can check if an error occurred
	defer func() {
		if returnedError != nil {
			if err := w.stopPortForward(ctx, pf); err != nil {
				log.WithError(err).Warn("failed to cleanup failed tunnel")
			}
		}
	}()

	ports := make([]string, len(req.Ports))
	for i, p := range req.Ports {
		portStr := strconv.Itoa(p)
		ports[i] = portStr + ":" + portStr
	}

	// TODO: need to release on error
	ipAddress, err := w.ippool.AcquireIP(w.ipCidr)
	if err != nil {
		return errors.Wrap(err, "failed to allocate IP")
	}
	pf.IP = ipAddress.IP

	// We only need to create alias on darwin, on other platforms
	// lo0 becomes lo and routes the full /8
	if runtime.GOOS == "darwin" {
		args := []string{"lo0", "alias", ipAddress.IP.String(), "up"}
		if err := exec.Command("ifconfig", args...).Run(); err != nil {
			return errors.Wrap(err, "failed to create ip link")
		}
	}
	pf.Hostnames = req.Hostnames

	w.dns.AddHosts(ipAddress.IP.String(), req.Hostnames)
	if err := w.dns.Save(); err != nil {
		return errors.Wrap(err, "failed to save DNS changes")
	}

	transport, upgrader, err := spdy.RoundTripperFor(w.rest)
	if err != nil {
		return errors.Wrap(err, "failed to upgrade connection")
	}

	var pod *PodInfo
	if req.Endpoint == nil {
		podInfo, err := w.getPodForService(ctx, &req.Service)
		if err == nil {
			pod = &podInfo
		}
	} else {
		pod = req.Endpoint
	}

	// only create the tunnel if we found a pod, if we didn't
	// then it will be looked for by the reaper
	if pod != nil {
		log = log.WithField("endpoint", pod.Key())
		pf.Pod = *pod

		log.Info("creating tunnel")
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", w.k.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("portforward").URL())

		fw, err := portforward.NewOnAddresses(dialer, []string{ipAddress.IP.String()}, ports, ctx.Done(), nil, ioutil.Discard, ioutil.Discard)
		if err != nil {
			return errors.Wrap(err, "failed to create port-forward")
		}
		pf.pf = fw

		go func() {
			err := fw.ForwardPorts()

			// if context was canceled (exiting) then we can ignore the error
			select {
			case <-ctx.Done():
				return
			default:
			}

			// otherwise, recreate it
			w.reqChan <- PortForwardRequest{
				CreatePortForwardRequest: &CreatePortForwardRequest{
					Service:        req.Service,
					Hostnames:      req.Hostnames,
					Ports:          req.Ports,
					Recreate:       true,
					RecreateReason: fmt.Sprintf("%v", err),
				},
			}
		}()
	} else {
		log.Warn("skipping tunnel creation due to no endpoint being found")
		pf.Status = PortForwardStatusWaiting
		pf.StatusReason = "No endpoints were found."
	}

	// mark that this is allocated
	w.portForwards[req.Service.Key()] = pf

	return nil
}

func (w *worker) setPortForwardConnectionStatus(_ context.Context, si ServiceInfo, status PortForwardStatus, reason string) {
	key := si.Key()
	pf, ok := w.portForwards[key]
	if !ok {
		return
	}

	pf.Status = status
	pf.StatusReason = reason
	w.portForwards[key] = pf
}

func (w *worker) stopPortForward(_ context.Context, conn *PortForwardConnection) error {
	if conn.pf != nil {
		conn.pf.Close()
	}

	errs := make([]error, 0)
	err := w.ippool.ReleaseIPFromPrefix(w.ipCidr, conn.IP.String())
	if err != nil {
		errs = append(errs, errors.Wrap(err, "failed to release ip address"))
	}

	// If we are on a platform that needs aliases
	// then we need to remove it
	if runtime.GOOS == "darwin" {
		ipStr := conn.IP.String()
		args := []string{"lo0", "-alias", ipStr}
		if err := exec.Command("ifconfig", args...).Run(); err != nil {
			errs = append(errs, errors.Wrap(err, "failed to release ip alias"))
		}
	}

	w.dns.RemoveHosts(conn.Hostnames)
	err = w.dns.Save()
	if err != nil {
		errs = append(errs, errors.Wrap(err, "failed to remove hosts entry"))
	}

	// if we have errors, return them
	if len(errs) > 0 {
		strs := []string{}
		for _, err := range errs {
			strs = append(strs, err.Error())
		}
		return fmt.Errorf("%v", strs)
	}

	return nil
}

func (w *worker) DeletePortForward(ctx context.Context, req *DeletePortForwardRequest) error {
	serviceKey := req.Service.Key()
	log := w.log.WithField("service", serviceKey)

	// skip port-forwards that are already being managed
	if w.portForwards[serviceKey] == nil {
		return fmt.Errorf("no port-forward exists for this service")
	}

	if err := w.stopPortForward(ctx, w.portForwards[serviceKey]); err != nil {
		log.WithError(err).Warn("failed to cleanup port-forward")
	}

	// now mark it as not being allocated
	delete(w.portForwards, serviceKey)

	log.Info("stopped port-forward")

	return nil
}
