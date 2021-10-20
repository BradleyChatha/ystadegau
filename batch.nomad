job "ymfudwr" {
    datacenters = ["dc1"]
    type = "batch"

    group "ymfudwr" {
        task "ymfudwr" {
            driver = "exec"

            artifact {
                source = "https://bradley-chatha.s3.eu-west-2.amazonaws.com/artifacts/ymfudwr_dist.zip"
                destination = "."
            }

            config {
                command = "ymfudwr"
            }

            resources {
                cpu = 100
                memory = 20
            }
        }
    }
}