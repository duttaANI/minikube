/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package bootstrapper

import (
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juju/mutex/v2"
	"github.com/otiai10/copy"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/drivers/kic/oci"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/kubeconfig"
	"k8s.io/minikube/pkg/minikube/localpath"
	"k8s.io/minikube/pkg/minikube/out"
	"k8s.io/minikube/pkg/minikube/vmpath"
	"k8s.io/minikube/pkg/util"
	"k8s.io/minikube/pkg/util/lock"
)

// SetupCerts gets the generated credentials required to talk to the APIServer.
func SetupCerts(cmd command.Runner, k8s config.ClusterConfig, n config.Node) error {
	localPath := localpath.Profile(k8s.KubernetesConfig.ClusterName)
	klog.Infof("Setting up %s for IP: %s\n", localPath, n.IP)

	ccs, regen, err := generateSharedCACerts()
	if err != nil {
		return errors.Wrap(err, "shared CA certs")
	}

	xfer, err := generateProfileCerts(k8s, n, ccs, regen)
	if err != nil {
		return errors.Wrap(err, "profile certs")
	}

	xfer = append(xfer, ccs.caCert)
	xfer = append(xfer, ccs.caKey)
	xfer = append(xfer, ccs.proxyCert)
	xfer = append(xfer, ccs.proxyKey)

	copyableFiles := []assets.CopyableFile{}
	defer func() {
		for _, f := range copyableFiles {
			if err := f.Close(); err != nil {
				klog.Warningf("error closing the file %s: %v", f.GetSourcePath(), err)
			}
		}
	}()

	for _, p := range xfer {
		cert := filepath.Base(p)
		perms := "0644"
		if strings.HasSuffix(cert, ".key") {
			perms = "0600"
		}
		certFile, err := assets.NewFileAsset(p, vmpath.GuestKubernetesCertsDir, cert, perms)
		if err != nil {
			return errors.Wrapf(err, "key asset %s", cert)
		}
		copyableFiles = append(copyableFiles, certFile)
	}

	caCerts, err := collectCACerts()
	if err != nil {
		return err
	}
	for src, dst := range caCerts {
		certFile, err := assets.NewFileAsset(src, path.Dir(dst), path.Base(dst), "0644")
		if err != nil {
			return errors.Wrapf(err, "ca asset %s", src)
		}

		copyableFiles = append(copyableFiles, certFile)
	}

	kcs := &kubeconfig.Settings{
		ClusterName:          n.Name,
		ClusterServerAddress: fmt.Sprintf("https://%s", net.JoinHostPort("localhost", fmt.Sprint(n.Port))),
		ClientCertificate:    path.Join(vmpath.GuestKubernetesCertsDir, "apiserver.crt"),
		ClientKey:            path.Join(vmpath.GuestKubernetesCertsDir, "apiserver.key"),
		CertificateAuthority: path.Join(vmpath.GuestKubernetesCertsDir, "ca.crt"),
		ExtensionContext:     kubeconfig.NewExtension(),
		ExtensionCluster:     kubeconfig.NewExtension(),
		KeepContext:          false,
	}

	kubeCfg := api.NewConfig()
	err = kubeconfig.PopulateFromSettings(kcs, kubeCfg)
	if err != nil {
		return errors.Wrap(err, "populating kubeconfig")
	}
	data, err := runtime.Encode(latest.Codec, kubeCfg)
	if err != nil {
		return errors.Wrap(err, "encoding kubeconfig")
	}

	if n.ControlPlane {
		kubeCfgFile := assets.NewMemoryAsset(data, vmpath.GuestPersistentDir, "kubeconfig", "0644")
		copyableFiles = append(copyableFiles, kubeCfgFile)
	}

	for _, f := range copyableFiles {
		if err := cmd.Copy(f); err != nil {
			return errors.Wrapf(err, "Copy %s", f.GetSourcePath())
		}
	}

	if err := installCertSymlinks(cmd, caCerts); err != nil {
		return errors.Wrapf(err, "certificate symlinks")
	}

	if err := generateKubeadmCerts(cmd, k8s); err != nil {
		return fmt.Errorf("failed to renew kubeadm certs: %v", err)
	}
	return nil
}

// CACerts has cert and key for CA (and Proxy)
type CACerts struct {
	caCert    string
	caKey     string
	proxyCert string
	proxyKey  string
}

// generateSharedCACerts generates CA certs shared among profiles, but only if missing
func generateSharedCACerts() (CACerts, bool, error) {
	regenProfileCerts := false
	globalPath := localpath.MiniPath()
	cc := CACerts{
		caCert:    localpath.CACert(),
		caKey:     filepath.Join(globalPath, "ca.key"),
		proxyCert: filepath.Join(globalPath, "proxy-client-ca.crt"),
		proxyKey:  filepath.Join(globalPath, "proxy-client-ca.key"),
	}

	caCertSpecs := []struct {
		certPath string
		keyPath  string
		subject  string
	}{
		{ // client / apiserver CA
			certPath: cc.caCert,
			keyPath:  cc.caKey,
			subject:  "minikubeCA",
		},
		{ // proxy-client CA
			certPath: cc.proxyCert,
			keyPath:  cc.proxyKey,
			subject:  "proxyClientCA",
		},
	}

	// create a lock for "ca-certs" to avoid race condition over multiple minikube instances rewriting shared ca certs
	hold := filepath.Join(globalPath, "ca-certs")
	spec := lock.PathMutexSpec(hold)
	spec.Timeout = 1 * time.Minute
	klog.Infof("acquiring lock for shared ca certs: %+v", spec)
	releaser, err := mutex.Acquire(spec)
	if err != nil {
		return cc, false, errors.Wrapf(err, "unable to acquire lock for shared ca certs %+v", spec)
	}
	defer releaser.Release()

	for _, ca := range caCertSpecs {
		if isValid(ca.certPath, ca.keyPath) {
			klog.Infof("skipping %s CA generation: %s", ca.subject, ca.keyPath)
			continue
		}

		regenProfileCerts = true
		klog.Infof("generating %s CA: %s", ca.subject, ca.keyPath)
		if err := util.GenerateCACert(ca.certPath, ca.keyPath, ca.subject); err != nil {
			return cc, false, errors.Wrap(err, "generate ca cert")
		}
	}

	return cc, regenProfileCerts, nil
}

// generateProfileCerts generates profile certs for a profile
func generateProfileCerts(cfg config.ClusterConfig, n config.Node, ccs CACerts, regen bool) ([]string, error) {

	// Only generate these certs for the api server
	if !n.ControlPlane {
		return []string{}, nil
	}

	k8s := cfg.KubernetesConfig
	profilePath := localpath.Profile(k8s.ClusterName)

	serviceIP, err := util.GetServiceClusterIP(k8s.ServiceCIDR)
	if err != nil {
		return nil, errors.Wrap(err, "getting service cluster ip")
	}

	apiServerIPs := k8s.APIServerIPs
	apiServerIPs = append(apiServerIPs,
		net.ParseIP(n.IP), serviceIP, net.ParseIP(oci.DefaultBindIPV4), net.ParseIP("10.0.0.1"))

	apiServerNames := k8s.APIServerNames
	apiServerNames = append(apiServerNames, k8s.APIServerName, constants.ControlPlaneAlias)

	apiServerAlternateNames := apiServerNames
	apiServerAlternateNames = append(apiServerAlternateNames,
		util.GetAlternateDNS(k8s.DNSDomain)...)

	daemonHost := oci.DaemonHost(k8s.ContainerRuntime)
	if daemonHost != oci.DefaultBindIPV4 {
		daemonHostIP := net.ParseIP(daemonHost)
		// if daemonHost is an IP we add it to the certificate's IPs, otherwise we assume it's an hostname and add it to the alternate names
		if daemonHostIP != nil {
			apiServerIPs = append(apiServerIPs, daemonHostIP)
		} else {
			apiServerAlternateNames = append(apiServerAlternateNames, daemonHost)
		}
	}

	// Generate a hash input for certs that depend on ip/name combinations
	hi := []string{}
	hi = append(hi, apiServerAlternateNames...)
	for _, ip := range apiServerIPs {
		hi = append(hi, ip.String())
	}
	sort.Strings(hi)

	specs := []struct {
		certPath string
		keyPath  string
		hash     string

		subject        string
		ips            []net.IP
		alternateNames []string
		caCertPath     string
		caKeyPath      string
	}{
		{ // Client cert
			certPath:       localpath.ClientCert(k8s.ClusterName),
			keyPath:        localpath.ClientKey(k8s.ClusterName),
			subject:        "minikube-user",
			ips:            []net.IP{},
			alternateNames: []string{},
			caCertPath:     ccs.caCert,
			caKeyPath:      ccs.caKey,
		},
		{ // apiserver serving cert
			hash:           fmt.Sprintf("%x", sha1.Sum([]byte(strings.Join(hi, "/"))))[0:8],
			certPath:       filepath.Join(profilePath, "apiserver.crt"),
			keyPath:        filepath.Join(profilePath, "apiserver.key"),
			subject:        "minikube",
			ips:            apiServerIPs,
			alternateNames: apiServerAlternateNames,
			caCertPath:     ccs.caCert,
			caKeyPath:      ccs.caKey,
		},
		{ // aggregator proxy-client cert
			certPath:       filepath.Join(profilePath, "proxy-client.crt"),
			keyPath:        filepath.Join(profilePath, "proxy-client.key"),
			subject:        "aggregator",
			ips:            []net.IP{},
			alternateNames: []string{},
			caCertPath:     ccs.proxyCert,
			caKeyPath:      ccs.proxyKey,
		},
	}

	xfer := []string{}
	for _, spec := range specs {
		if spec.subject != "minikube-user" {
			xfer = append(xfer, spec.certPath)
			xfer = append(xfer, spec.keyPath)
		}

		cp := spec.certPath
		kp := spec.keyPath
		if spec.hash != "" {
			cp = cp + "." + spec.hash
			kp = kp + "." + spec.hash
		}

		if !regen && isValid(cp, kp) {
			klog.Infof("skipping %s signed cert generation: %s", spec.subject, kp)
			continue
		}

		klog.Infof("generating %s signed cert: %s", spec.subject, kp)
		if canRead(cp) {
			os.Remove(cp)
		}
		if canRead(kp) {
			os.Remove(kp)
		}
		err := util.GenerateSignedCert(
			cp, kp, spec.subject,
			spec.ips, spec.alternateNames,
			spec.caCertPath, spec.caKeyPath,
			cfg.CertExpiration,
		)
		if err != nil {
			return xfer, errors.Wrapf(err, "generate signed cert for %q", spec.subject)
		}

		if spec.hash != "" {
			klog.Infof("copying %s -> %s", cp, spec.certPath)
			if err := copy.Copy(cp, spec.certPath); err != nil {
				return xfer, errors.Wrap(err, "copy cert")
			}
			klog.Infof("copying %s -> %s", kp, spec.keyPath)
			if err := copy.Copy(kp, spec.keyPath); err != nil {
				return xfer, errors.Wrap(err, "copy key")
			}
		}
	}

	return xfer, nil
}

func generateKubeadmCerts(cmd command.Runner, cc config.ClusterConfig) error {
	needsRefresh := false
	certs := []string{"apiserver-etcd-client", "apiserver-kubelet-client", "etcd-server", "etcd-healthcheck-client", "etcd-peer", "front-proxy-client"}
	for _, cert := range certs {
		certPath := []string{vmpath.GuestPersistentDir, "certs"}
		// certs starting with "etcd-" are in the "etcd" dir
		// ex: etcd-server => etcd/server
		if strings.HasPrefix(cert, "etcd-") {
			certPath = append(certPath, "etcd")
		}
		certPath = append(certPath, strings.TrimPrefix(cert, "etcd-")+".crt")
		if !isKubeadmCertValid(cmd, path.Join(certPath...)) {
			needsRefresh = true
		}
	}
	if !needsRefresh {
		return nil
	}
	out.WarningT("kubeadm certificates have expired. Generating new ones...")
	kubeadmPath := path.Join(vmpath.GuestPersistentDir, "binaries", cc.KubernetesConfig.KubernetesVersion)
	bashCmd := fmt.Sprintf("sudo env PATH=\"%s:$PATH\" kubeadm certs renew all --config %s", kubeadmPath, constants.KubeadmYamlPath)
	if _, err := cmd.RunCmd(exec.Command("/bin/bash", "-c", bashCmd)); err != nil {
		return fmt.Errorf("failed to renew kubeadm certs: %v", err)
	}
	return nil
}

// isValidPEMCertificate checks whether the input file is a valid PEM certificate (with at least one CERTIFICATE block)
func isValidPEMCertificate(filePath string) (bool, error) {
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}

	for {
		block, rest := pem.Decode(fileBytes)
		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			// certificate found
			return true, nil
		}
		fileBytes = rest
	}

	return false, nil
}

// collectCACerts looks up all PEM certificates with .crt or .pem extension in ~/.minikube/certs or ~/.minikube/files/etc/ssl/certs to copy to the host.
// minikube root CA is also included but libmachine certificates (ca.pem/cert.pem) are excluded.
func collectCACerts() (map[string]string, error) {
	localPath := localpath.MiniPath()
	certFiles := map[string]string{}

	dirs := []string{filepath.Join(localPath, "certs"), filepath.Join(localPath, "files", "etc", "ssl", "certs")}
	for _, certsDir := range dirs {
		err := filepath.Walk(certsDir, func(hostpath string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info == nil {
				return nil
			}
			if info.IsDir() {
				return nil
			}

			fullPath := filepath.Join(certsDir, hostpath)
			ext := strings.ToLower(filepath.Ext(hostpath))

			if ext == ".crt" || ext == ".pem" {
				if info.Size() < 32 {
					klog.Warningf("ignoring %s, impossibly tiny %d bytes", fullPath, info.Size())
					return nil
				}

				klog.Infof("found cert: %s (%d bytes)", fullPath, info.Size())

				validPem, err := isValidPEMCertificate(hostpath)
				if err != nil {
					return err
				}

				if validPem {
					filename := filepath.Base(hostpath)
					dst := fmt.Sprintf("%s.%s", strings.TrimSuffix(filename, ext), "pem")
					certFiles[hostpath] = path.Join(vmpath.GuestCertAuthDir, dst)
				}
			}
			return nil
		})

		if err != nil {
			return nil, errors.Wrapf(err, "provisioning: traversal certificates dir %s", certsDir)
		}

		for _, excluded := range []string{"ca.pem", "cert.pem"} {
			certFiles[filepath.Join(certsDir, excluded)] = ""
		}
	}

	// populates minikube CA
	certFiles[filepath.Join(localPath, "ca.crt")] = path.Join(vmpath.GuestCertAuthDir, "minikubeCA.pem")

	filtered := map[string]string{}
	for k, v := range certFiles {
		if v != "" {
			filtered[k] = v
		}
	}
	return filtered, nil
}

// getSubjectHash calculates Certificate Subject Hash for creating certificate symlinks
func getSubjectHash(cr command.Runner, filePath string) (string, error) {
	lrr, err := cr.RunCmd(exec.Command("ls", "-la", filePath))
	if err != nil {
		return "", err
	}
	klog.Infof("hashing: %s", lrr.Stdout.String())

	rr, err := cr.RunCmd(exec.Command("openssl", "x509", "-hash", "-noout", "-in", filePath))
	if err != nil {
		crr, _ := cr.RunCmd(exec.Command("cat", filePath))
		return "", errors.Wrapf(err, "cert:\n%s\n---\n%s", lrr.Output(), crr.Stdout.String())
	}
	stringHash := strings.TrimSpace(rr.Stdout.String())
	return stringHash, nil
}

// installCertSymlinks installs certs in /usr/share/ca-certificates into system-wide certificate store (/etc/ssl/certs).
// OpenSSL binary required in minikube ISO
func installCertSymlinks(cr command.Runner, caCerts map[string]string) error {
	hasSSLBinary := true
	_, err := cr.RunCmd(exec.Command("openssl", "version"))
	if err != nil {
		hasSSLBinary = false
	}

	if !hasSSLBinary && len(caCerts) > 0 {
		klog.Warning("OpenSSL not found. Please recreate the cluster with the latest minikube ISO.")
	}

	for _, caCertFile := range caCerts {
		dstFilename := path.Base(caCertFile)
		certStorePath := path.Join(vmpath.GuestCertStoreDir, dstFilename)

		cmd := fmt.Sprintf("test -s %s && ln -fs %s %s", caCertFile, caCertFile, certStorePath)
		if _, err := cr.RunCmd(exec.Command("sudo", "/bin/bash", "-c", cmd)); err != nil {
			return errors.Wrapf(err, "create symlink for %s", caCertFile)
		}

		if !hasSSLBinary {
			continue
		}

		subjectHash, err := getSubjectHash(cr, caCertFile)
		if err != nil {
			return errors.Wrapf(err, "calculate hash for cacert %s", caCertFile)
		}
		subjectHashLink := path.Join(vmpath.GuestCertStoreDir, fmt.Sprintf("%s.0", subjectHash))

		// NOTE: This symlink may exist, but point to a missing file
		cmd = fmt.Sprintf("test -L %s || ln -fs %s %s", subjectHashLink, certStorePath, subjectHashLink)
		if _, err := cr.RunCmd(exec.Command("sudo", "/bin/bash", "-c", cmd)); err != nil {
			return errors.Wrapf(err, "create symlink for %s", caCertFile)
		}
	}
	return nil
}

// canRead returns true if the file represented
// by path exists and is readable, otherwise false.
func canRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return true
}

// isValid checks a cert/key path and makes sure it's still valid
// if a cert is expired or otherwise invalid, it will be deleted
func isValid(certPath, keyPath string) bool {
	if !canRead(keyPath) {
		return false
	}

	certFile, err := os.ReadFile(certPath)
	if err != nil {
		klog.Infof("failed to read cert file %s: %v", certPath, err)
		os.Remove(certPath)
		os.Remove(keyPath)
		return false
	}

	certData, _ := pem.Decode(certFile)
	if certData == nil {
		klog.Infof("failed to decode cert file %s", certPath)
		os.Remove(certPath)
		os.Remove(keyPath)
		return false
	}

	cert, err := x509.ParseCertificate(certData.Bytes)
	if err != nil {
		klog.Infof("failed to parse cert file %s: %v\n", certPath, err)
		os.Remove(certPath)
		os.Remove(keyPath)
		return false
	}

	if cert.NotAfter.Before(time.Now()) {
		out.WarningT("Certificate {{.certPath}} has expired. Generating a new one...", out.V{"certPath": filepath.Base(certPath)})
		klog.Infof("cert expired %s: expiration: %s, now: %s", certPath, cert.NotAfter, time.Now())
		os.Remove(certPath)
		os.Remove(keyPath)
		return false
	}

	return true
}

func isKubeadmCertValid(cmd command.Runner, certPath string) bool {
	rr, err := cmd.RunCmd(exec.Command("cat", certPath))
	if err != nil {
		klog.Infof("failed to read cert file %s: %v", certPath, err)
		// if reading the cert failed it's likely first start and it doesn't exist yet so mark as valid
		return true
	}

	certData, _ := pem.Decode(rr.Stdout.Bytes())
	if certData == nil {
		klog.Infof("failed to decode cert file %s", certPath)
		return false
	}

	cert, err := x509.ParseCertificate(certData.Bytes)
	if err != nil {
		klog.Infof("failed to parse cert file %s: %v\n", certPath, err)
		return false
	}

	if cert.NotAfter.Before(time.Now()) {
		klog.Infof("cert expired %s: expiration: %s, now: %s", certPath, cert.NotAfter, time.Now())
		return false
	}

	return true
}
