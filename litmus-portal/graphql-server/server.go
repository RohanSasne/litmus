package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/kelseyhightower/envconfig"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/graph"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/graph/generated"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/authorization"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/chaoshub"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/cluster"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/database/mongodb"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/database/mongodb/config"
	gitOpsHandler "github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/gitops/handler"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/projects"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/rest_handlers"
	pb "github.com/litmuschaos/litmus/litmus-portal/graphql-server/protos"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/utils"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

func init() {
	err := envconfig.Process("", &utils.Config)

	// Default log format is text
	if utils.Config.LitmusChaosServerLogFormat == "json" {
		log.SetFormatter(&log.JSONFormatter{})
	}
	log.SetReportCaller(true)

	log.Infof("go version: %s", runtime.Version())
	log.Infof("go os/arch: %s/%s", runtime.GOOS, runtime.GOARCH)

	if err != nil {
		log.Fatal(err)
	}

	// confirm version env is valid
	if !strings.Contains(strings.ToLower(utils.Config.Version), cluster.CIVersion) {
		splitCPVersion := strings.Split(utils.Config.Version, ".")
		if len(splitCPVersion) != 3 {
			log.Fatal("version doesn't follow semver semantic")
		}
	}

	log.Infof("Version: %s", utils.Config.Version)
}

func validateVersion() error {
	currentVersion := utils.Config.Version
	dbVersion, err := config.GetConfig(context.Background(), "version")
	if err != nil {
		return fmt.Errorf("failed to get version from db, error = %w", err)
	}
	if dbVersion == nil {
		err := config.CreateConfig(context.Background(), &config.ServerConfig{Key: "version", Value: currentVersion})
		if err != nil {
			return fmt.Errorf("failed to insert current version in db, error = %w", err)
		}
		return nil
	}
	if dbVersion.Value.(string) != currentVersion {
		return fmt.Errorf("control plane needs to be upgraded from version %v to %v", dbVersion.Value.(string), currentVersion)
	}
	return nil
}

func main() {
	client, err := mongodb.MongoConnection()
	if err != nil {
		log.Fatal(err)
	}

	mongoClient := mongodb.Initialize(client)

	var mongodbOperator mongodb.MongoOperator = mongodb.NewMongoOperations(mongoClient)
	// TODO: remove this when all packages shift to interface pattern
	mongodb.Operator = mongodbOperator

	if err := validateVersion(); err != nil {
		log.Fatal(err)
	}

	go startGRPCServer(utils.Config.RpcPort, mongodbOperator) // start GRPC server

	srv := handler.New(generated.NewExecutableSchema(graph.NewConfig(mongodbOperator)))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	})

	// to be removed in production
	srv.Use(extension.Introspection{})

	gin.SetMode(gin.ReleaseMode)
	gin.EnableJsonDecoderDisallowUnknownFields()
	router := gin.New()
	router.Use(rest_handlers.LoggingMiddleware())
	router.Use(gin.Recovery())
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowHeaders:     []string{"*"},
		AllowCredentials: true,
	}))

	// routers
	router.GET("/", rest_handlers.PlaygroundHandler())
	router.Any("/query", authorization.Middleware(srv))
	router.GET("/readiness", rest_handlers.ReadinessHandler(client))
	router.GET("/icon/:ProjectID/:HubName/:ChartName/:IconName", authorization.RestMiddlewareWithRole(chaoshub.GetIconHandler, nil))
	router.Any("/file/:key", rest_handlers.FileHandler)
	router.GET("/status", rest_handlers.StatusHandler)
	router.GET("/workflow_helper_image_version", rest_handlers.WorkflowHelperImageVersionHandler)

	gitOpsHandler.GitOpsSyncHandler(true) // sync all previous existing repos before start

	go chaoshub.NewService(mongodbOperator).RecurringHubSync() // go routine for syncing hubs for all users
	go gitOpsHandler.GitOpsSyncHandler(false)                  // routine to sync git repos for gitOps

	log.Infof("connect to http://localhost:%s/ for GraphQL playground", utils.Config.HttpPort)
	err = router.Run(":" + utils.Config.HttpPort)
	if err != nil {
		log.Fatal(err)
	}
}

// startGRPCServer initializes, registers services to and starts the gRPC server for RPC calls
func startGRPCServer(port string, mongodbOperator mongodb.MongoOperator) {
	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal("failed to listen: %w", err)
	}

	log.ErrorKey = "grpc.error"
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_logrus.UnaryServerInterceptor(log.NewEntry(log.StandardLogger())),
		)),
	)

	// Register services
	pb.RegisterProjectServer(grpcServer, &projects.ProjectServer{Operator: mongodbOperator})

	log.Infof("GRPC server listening on %v", lis.Addr())
	log.Fatal(grpcServer.Serve(lis))
}
