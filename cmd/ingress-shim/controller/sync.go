package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
)

const (
	// tlsACMEAnnotation is here for compatibility with kube-lego style
	// ingress resources. When set to "true", a Certificate resource with
	// the default configuration provided to ingress-annotation should be
	// created.
	tlsACMEAnnotation = "kubernetes.io/tls-acme"
	// issuerNameAnnotation can be used to override the issuer specified on the
	// created Certificate resource.
	issuerNameAnnotation = "certmanager.k8s.io/issuer"
	// clusterIssuerNameAnnotation can be used to override the issuer specified on the
	// created Certificate resource. The Certificate will reference the
	// specified *ClusterIssuer* instead of normal issuer.
	clusterIssuerNameAnnotation = "certmanager.k8s.io/cluster-issuer"
	// acmeIssuerChallengeTypeAnnotation can be used to override the default ACME challenge
	// type to be used when the specified issuer is an ACME issuer
	acmeIssuerChallengeTypeAnnotation = "certmanager.k8s.io/acme-challenge-type"
	// acmeIssuerDNS01ProviderNameAnnotation can be used to override the default dns01 provider
	// configured on the issuer if the challenge type is set to dns01
	acmeIssuerDNS01ProviderNameAnnotation = "certmanager.k8s.io/acme-dns01-provider"
)

var ingressGVK = extv1beta1.SchemeGroupVersion.WithKind("Ingress")

func (c *Controller) Sync(ctx context.Context, ing *extv1beta1.Ingress) error {
	if !shouldSync(ing) {
		glog.Infof("Not syncing ingress %s/%s as it does not contain necessary annotations", ing.Namespace, ing.Name)
		return nil
	}

	crts, err := c.buildCertificates(ing)
	if err != nil {
		return err
	}

	for _, crt := range crts {
		_, err := c.CMClient.CertmanagerV1alpha1().Certificates(crt.Namespace).Create(crt)
		if err != nil {
			return err
		}
		c.Recorder.Eventf(ing, corev1.EventTypeNormal, "CreateCertificate", "Successfully created Certificate %q", crt.Name)
	}

	return nil
}

func (c *Controller) buildCertificates(ing *extv1beta1.Ingress) ([]*v1alpha1.Certificate, error) {
	issuerName, issuerKind := c.issuerForIngress(ing)
	issuer, err := c.getGenericIssuer(ing.Namespace, issuerName, issuerKind)
	if err != nil {
		return nil, err
	}

	var crts []*v1alpha1.Certificate
	for i, tls := range ing.Spec.TLS {
		// validate the ingress TLS block
		if len(tls.Hosts) == 0 {
			return nil, fmt.Errorf("secret %q for ingress %q has no hosts specified", tls.SecretName, ing.Name)
		}
		if tls.SecretName == "" {
			return nil, fmt.Errorf("TLS entry %d for ingress %q must specify a secretName", i, ing.Name)
		}

		// check if a Certificate for this TLS entry already exists, and if it
		// does then skip this entry
		existingCrt, err := c.certificateLister.Certificates(ing.Namespace).Get(tls.SecretName)
		if !apierrors.IsNotFound(err) && err != nil {
			return nil, err
		}
		if existingCrt != nil {
			glog.Infof("Certificate %q for ingress %q already exists, not re-creating", tls.SecretName, ing.Name)
			continue
		}

		crt := &v1alpha1.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name:            tls.SecretName,
				Namespace:       ing.Namespace,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ing, ingressGVK)},
			},
			Spec: v1alpha1.CertificateSpec{
				DNSNames:   tls.Hosts,
				SecretName: tls.SecretName,
				IssuerRef: v1alpha1.ObjectReference{
					Name: issuerName,
					Kind: issuerKind,
				},
			},
		}
		err = c.setIssuerSpecificConfig(crt, issuer, ing, tls)
		if err != nil {
			return nil, err
		}
		crts = append(crts, crt)
	}
	return crts, nil
}

func (c *Controller) setIssuerSpecificConfig(crt *v1alpha1.Certificate, issuer v1alpha1.GenericIssuer, ing *extv1beta1.Ingress, tls extv1beta1.IngressTLS) error {
	ingAnnotations := ing.Annotations
	if ingAnnotations == nil {
		ingAnnotations = map[string]string{}
	}
	// for ACME issuers
	if issuer.GetSpec().ACME != nil {
		challengeType, ok := ingAnnotations[acmeIssuerChallengeTypeAnnotation]
		if !ok {
			challengeType = c.options.DefaultACMEIssuerChallengeType
		}
		domainCfg := v1alpha1.ACMECertificateDomainConfig{
			Domains: tls.Hosts,
		}
		switch challengeType {
		case "http01":
			domainCfg.HTTP01 = &v1alpha1.ACMECertificateHTTP01Config{Ingress: ing.Name}
		case "dns01":
			dnsProvider, ok := ingAnnotations[acmeIssuerDNS01ProviderNameAnnotation]
			if !ok {
				dnsProvider = c.options.DefaultACMEIssuerDNS01ProviderName
			}
			if dnsProvider == "" {
				return fmt.Errorf("no acme issuer dns01 challenge provider specified")
			}
			domainCfg.DNS01 = &v1alpha1.ACMECertificateDNS01Config{Provider: dnsProvider}
		default:
			return fmt.Errorf("invalid acme issuer challenge type specified %q", challengeType)
		}
		crt.Spec.ACME = &v1alpha1.ACMECertificateConfig{Config: []v1alpha1.ACMECertificateDomainConfig{domainCfg}}
	}
	return nil
}

// shouldSync returns true if this ingress should have a Certificate resource
// created for it
func shouldSync(ing *extv1beta1.Ingress) bool {
	annotations := ing.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, ok := annotations[issuerNameAnnotation]; ok {
		return true
	}
	if _, ok := annotations[clusterIssuerNameAnnotation]; ok {
		return true
	}
	if s, ok := annotations[tlsACMEAnnotation]; ok {
		if b, _ := strconv.ParseBool(s); b {
			return true
		}
	}
	if _, ok := annotations[acmeIssuerChallengeTypeAnnotation]; ok {
		return true
	}
	if _, ok := annotations[acmeIssuerDNS01ProviderNameAnnotation]; ok {
		return true
	}
	return false
}

// issuerForIngress will determine the issuer that should be specified on a
// Certificate created for the given Ingress resource. If one is not set, the
// default issuer given to the controller will be used.
func (c *Controller) issuerForIngress(ing *extv1beta1.Ingress) (name string, kind string) {
	name = c.options.DefaultIssuerName
	kind = c.options.DefaultIssuerKind
	annotations := ing.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	if issuerName, ok := annotations[issuerNameAnnotation]; ok {
		name = issuerName
		kind = v1alpha1.IssuerKind
	}
	if issuerName, ok := annotations[clusterIssuerNameAnnotation]; ok {
		name = issuerName
		kind = v1alpha1.ClusterIssuerKind
	}
	return name, kind
}

func (c *Controller) getGenericIssuer(namespace, name, kind string) (v1alpha1.GenericIssuer, error) {
	switch kind {
	case v1alpha1.IssuerKind:
		return c.issuerLister.Issuers(namespace).Get(name)
	case v1alpha1.ClusterIssuerKind:
		if c.clusterIssuerLister == nil {
			return nil, fmt.Errorf("cannot get ClusterIssuer for %q as ingress-shim is scoped to a single namespace", name)
		}
		return c.clusterIssuerLister.Get(name)
	default:
		return nil, fmt.Errorf(`invalid value %q for issuer kind. Must be empty, %q or %q`, kind, v1alpha1.IssuerKind, v1alpha1.ClusterIssuerKind)
	}
}
