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

package security

import (
	"crypto/tls"
	"github.com/apache/dubbo-admin/ca/pkg/cert"
	"github.com/apache/dubbo-admin/ca/pkg/config"
	"github.com/apache/dubbo-admin/ca/pkg/k8s"
	"github.com/apache/dubbo-admin/ca/pkg/logger"
	"github.com/apache/dubbo-admin/ca/pkg/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"time"
)

type Server struct {
	StopChan chan os.Signal

	Options     *config.Options
	CertStorage *cert.Storage

	KubeClient k8s.Client

	CertificateServer *v1alpha1.DubboCertificateServiceServerImpl
	PlainServer       *grpc.Server
	SecureServer      *grpc.Server
}

func NewServer(options *config.Options) *Server {
	return &Server{
		Options:  options,
		StopChan: make(chan os.Signal, 1),
	}
}

func (s *Server) Init() {
	// TODO bypass k8s work
	if s.KubeClient == nil {
		s.KubeClient = k8s.NewClient()
	}
	if !s.KubeClient.Init() {
		panic("Failed to create kubernetes client.")
	}

	s.CertStorage = cert.NewStorage(s.Options)
	s.StopChan = s.StopChan
	go s.CertStorage.RefreshServerCert()

	// TODO inject pod based on Webhook

	s.LoadRootCert()
	s.LoadAuthorityCert()

	impl := &v1alpha1.DubboCertificateServiceServerImpl{
		Options:     s.Options,
		CertStorage: s.CertStorage,
		KubeClient:  s.KubeClient,
	}

	s.PlainServer = grpc.NewServer()
	v1alpha1.RegisterDubboCertificateServiceServer(s.PlainServer, impl)
	reflection.Register(s.PlainServer)

	tlsConfig := &tls.Config{
		GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.CertStorage.GetServerCert(info.ServerName), nil
		},
	}

	s.CertStorage.GetServerCert("localhost")
	s.CertStorage.GetServerCert("dubbo-ca." + s.Options.Namespace + ".svc")
	s.CertStorage.GetServerCert("dubbo-ca." + s.Options.Namespace + ".svc.cluster.local")

	s.SecureServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	v1alpha1.RegisterDubboCertificateServiceServer(s.SecureServer, impl)
	reflection.Register(s.SecureServer)
}

func (s *Server) LoadRootCert() {
	// todo
}

func (s *Server) LoadAuthorityCert() {
	certStr, priStr := s.KubeClient.GetAuthorityCert(s.Options.Namespace)
	if certStr != "" && priStr != "" {
		s.CertStorage.AuthorityCert.Cert = cert.DecodeCert(certStr)
		s.CertStorage.AuthorityCert.CertPem = certStr
		s.CertStorage.AuthorityCert.PrivateKey = cert.DecodePrivateKey(priStr)
	}

	s.RefreshAuthorityCert()
	go s.ScheduleRefreshAuthorityCert()
}

func (s *Server) ScheduleRefreshAuthorityCert() {
	interval := math.Min(math.Floor(float64(s.Options.CaValidity)/100), 10_000)
	for true {
		time.Sleep(time.Duration(interval) * time.Millisecond)
		if s.CertStorage.AuthorityCert.NeedRefresh() {
			logger.Sugar.Infof("Authority cert is invalid, refresh it.")
			// TODO lock if multi server
			// TODO refresh signed cert
			s.CertStorage.AuthorityCert = cert.GenerateAuthorityCert(s.CertStorage.RootCert, s.Options.CaValidity)
			s.KubeClient.UpdateAuthorityCert(s.CertStorage.AuthorityCert.CertPem, cert.EncodePri(s.CertStorage.AuthorityCert.PrivateKey), s.Options.Namespace)
			if s.KubeClient.UpdateAuthorityPublicKey(s.CertStorage.AuthorityCert.CertPem) {
				logger.Sugar.Infof("Write ca to config maps success.")
			} else {
				logger.Sugar.Warnf("Write ca to config maps failed.")
			}
		}

		select {
		case <-s.StopChan:
			return
		default:
			continue
		}
	}
}

func (s *Server) RefreshAuthorityCert() {
	if s.CertStorage.AuthorityCert.IsValid() {
		logger.Sugar.Infof("Load authority cert from kubernetes secrect success.")
	} else {
		logger.Sugar.Warnf("Load authority cert from kubernetes secrect failed.")
		s.CertStorage.AuthorityCert = cert.GenerateAuthorityCert(s.CertStorage.RootCert, s.Options.CaValidity)

		// TODO lock if multi server
		s.KubeClient.UpdateAuthorityCert(s.CertStorage.AuthorityCert.CertPem, cert.EncodePri(s.CertStorage.AuthorityCert.PrivateKey), s.Options.Namespace)
	}

	// TODO add task to update ca
	logger.Sugar.Info("Writing ca to config maps.")
	if s.KubeClient.UpdateAuthorityPublicKey(s.CertStorage.AuthorityCert.CertPem) {
		logger.Sugar.Info("Write ca to config maps success.")
	} else {
		logger.Sugar.Warnf("Write ca to config maps failed.")
	}
	s.CertStorage.TrustedCert = append(s.CertStorage.TrustedCert, s.CertStorage.AuthorityCert)
}

func (s *Server) Start() {
	lis, err := net.Listen("tcp", ":"+strconv.Itoa(s.Options.PlainServerPort))
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		err := s.PlainServer.Serve(lis)
		if err != nil {
			log.Fatal(err)
		}
	}()
	lis, err = net.Listen("tcp", ":"+strconv.Itoa(s.Options.SecureServerPort))
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		err := s.SecureServer.Serve(lis)
		if err != nil {
			log.Fatal(err)
		}
	}()

	logger.Sugar.Info("Server started.")
}
