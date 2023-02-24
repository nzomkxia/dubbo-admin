// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"flag"
	"github.com/apache/dubbo-admin/ca/pkg/logger"
	k8sauth "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"path/filepath"
)

type Client interface {
	Init() bool
	GetAuthorityCert(namespace string) (string, string)
	UpdateAuthorityCert(cert string, pri string, namespace string)
	UpdateAuthorityPublicKey(cert string) bool
	VerifyServiceAccount(token string) bool
}

type ClientImpl struct {
	kubeClient *kubernetes.Clientset
}

func NewClient() Client {
	return &ClientImpl{}
}

func (c *ClientImpl) Init() bool {
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Sugar.Infof("Failed to load config from Pod. Will fall back to kube config file.")

		var kubeconfig *string
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
		flag.Parse()

		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			logger.Sugar.Warnf("Failed to load config from kube config file.")
			return false
		}
	}

	// creates the clientset
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Sugar.Warnf("Failed to create client to kubernetes. " + err.Error())
		return false
	}
	c.kubeClient = clientSet
	return true
}

func (c *ClientImpl) GetAuthorityCert(namespace string) (string, string) {
	s, err := c.kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), "dubbo-ca-secret", metav1.GetOptions{})
	if err != nil {
		logger.Sugar.Warnf("Unable to get authority cert secret from kubernetes. " + err.Error())
	}
	return string(s.Data["cert.pem"]), string(s.Data["pri.pem"])
}

func (c *ClientImpl) UpdateAuthorityCert(cert string, pri string, namespace string) {
	s, err := c.kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), "dubbo-ca-secret", metav1.GetOptions{})
	if err != nil {
		logger.Sugar.Warnf("Unable to get ca secret from kubernetes. Will try to create. " + err.Error())
		s = &v1.Secret{
			Data: map[string][]byte{
				"cert.pem": []byte(cert),
				"pri.pem":  []byte(pri),
			},
		}
		s.Name = "dubbo-ca-secret"
		_, err = c.kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), s, metav1.CreateOptions{})
		if err != nil {
			logger.Sugar.Warnf("Failed to create ca secret to kubernetes. " + err.Error())
		} else {
			logger.Sugar.Info("Create ca secret to kubernetes success. ")
		}
	}

	if string(s.Data["cert.pem"]) == cert && string(s.Data["pri.pem"]) == pri {
		logger.Sugar.Info("Ca secret in kubernetes is already the newest vesion.")
		return
	}

	s.Data["cert.pem"] = []byte(cert)
	s.Data["pri.pem"] = []byte(pri)
	_, err = c.kubeClient.CoreV1().Secrets(namespace).Update(context.TODO(), s, metav1.UpdateOptions{})
	if err != nil {
		logger.Sugar.Warnf("Failed to update ca secret to kubernetes. " + err.Error())
	} else {
		logger.Sugar.Info("Update ca secret to kubernetes success. ")
	}
}

func (c *ClientImpl) UpdateAuthorityPublicKey(cert string) bool {
	ns, err := c.kubeClient.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logger.Sugar.Warnf("Failed to get namespaces. " + err.Error())
		return false
	}
	for _, n := range ns.Items {
		if n.Name == "kube-system" {
			continue
		}
		cm, err := c.kubeClient.CoreV1().ConfigMaps(n.Name).Get(context.TODO(), "dubbo-ca-cert", metav1.GetOptions{})
		if err != nil {
			logger.Sugar.Warnf("Unable to find dubbo-ca-cert in " + n.Name + ". Will create config map. " + err.Error())
			cm = &v1.ConfigMap{
				Data: map[string]string{
					"ca.crt": cert,
				},
			}
			cm.Name = "dubbo-ca-cert"
			_, err = c.kubeClient.CoreV1().ConfigMaps(n.Name).Create(context.TODO(), cm, metav1.CreateOptions{})
			if err != nil {
				logger.Sugar.Warnf("Failed to create config map for " + n.Name + ". " + err.Error())
				return false
			} else {
				logger.Sugar.Info("Create ca config map for " + n.Name + " success.")
			}
		}
		if cm.Data["ca.crt"] == cert {
			logger.Sugar.Info("Ignore override ca to " + n.Name + ". Cause: Already exist.")
			continue
		}
		cm.Data["ca.crt"] = cert
		_, err = c.kubeClient.CoreV1().ConfigMaps(n.Name).Update(context.TODO(), cm, metav1.UpdateOptions{})
		if err != nil {
			logger.Sugar.Warnf("Failed to update config map for " + n.Name + ". " + err.Error())
			return false
		} else {
			logger.Sugar.Info("Update ca config map for " + n.Name + " success.")
		}
	}
	return true
}

func (c *ClientImpl) VerifyServiceAccount(token string) bool {
	tokenReview := &k8sauth.TokenReview{
		Spec: k8sauth.TokenReviewSpec{
			Token: token,
		},
	}
	reviewRes, err := c.kubeClient.AuthenticationV1().TokenReviews().Create(context.TODO(), tokenReview, metav1.CreateOptions{})
	if err != nil {
		logger.Sugar.Warnf("Failed to validate token. " + err.Error())
		return false
	}
	// TODO support aud
	if reviewRes.Status.Error != "" {
		logger.Sugar.Warnf("Failed to validate token. " + reviewRes.Status.Error)
		return false
	}
	return true
}
