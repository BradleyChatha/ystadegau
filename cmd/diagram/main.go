package main

import (
	"log"
	"os"
	"os/exec"

	"github.com/blushft/go-diagrams/diagram"
	"github.com/blushft/go-diagrams/nodes/aws"
)

func main() {
	os.RemoveAll("go-diagrams")
	renderDesired()
	os.Chdir("go-diagrams")
	log.Print(exec.Command("dot", "-Tpng", "desired.dot", "-o../desired.png").Run())
	os.Chdir("..")
	os.RemoveAll("go-diagrams")
	renderActual()
	os.Chdir("go-diagrams")
	log.Print(exec.Command("dot", "-Tpng", "actual.dot", "-o../actual.png").Run())
}

func renderDesired() {
	d, err := diagram.New(diagram.Label("Desired Architecture"), diagram.Filename("desired"))
	if err != nil {
		log.Fatal(err)
	}

	vpc := aws.Network.Vpc(diagram.NodeLabel("VPC"))
	nat := aws.Network.NatGateway(diagram.NodeLabel("NAT"))
	rds := aws.Database.Rds(diagram.NodeLabel("Postgres"))
	agw := aws.Network.ApiGateway(diagram.NodeLabel("Api Gateway"))

	eb := aws.Integration.Eventbridge(diagram.NodeLabel("Event Bridge"))

	l1 := aws.Compute.Lambda(diagram.NodeLabel("Ymfudwr"))
	l2 := aws.Compute.Lambda(diagram.NodeLabel("Gwyliwr"))
	l3 := aws.Compute.Lambda(diagram.NodeLabel("Chwilwr"))
	lg := diagram.NewGroup("functions").
		Label("Functions").
		Add(
			l1,
			l2,
			l3,
		).
		ConnectAllFrom(nat.ID(), diagram.Bidirectional()).
		ConnectAllTo(rds.ID(), diagram.Forward())
	d.
		Connect(agw, l3, diagram.Bidirectional()).
		Connect(l1, eb, diagram.Forward()).
		Connect(l2, eb, diagram.Forward()).
		Connect(vpc, nat, diagram.Bidirectional()).
		Add(eb, rds, agw).
		Group(lg)

	err = d.Render()
	if err != nil {
		log.Fatal(err)
	}
}

func renderActual() {
	d, _ := diagram.New(diagram.Label("Actual Architecture"), diagram.Filename("actual"))

	vpc := aws.Network.Vpc(diagram.NodeLabel("VPC"))
	rds := aws.Database.Rds(diagram.NodeLabel("Postgres"))
	igw := aws.Network.InternetGateway(diagram.NodeLabel("Gateway"))

	eb := aws.Integration.Eventbridge(diagram.NodeLabel("Event Bridge"))
	ec := aws.Compute.Ec2(diagram.NodeLabel("Ymfudwr & Gwyliwr & Chwilwr"))
	d.
		Connect(vpc, igw, diagram.Bidirectional()).
		Connect(ec, igw, diagram.Bidirectional()).
		Connect(eb, ec, diagram.Bidirectional()).
		Connect(ec, rds, diagram.Forward())

	d.Render()
}
