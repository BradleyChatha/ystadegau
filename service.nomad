job "ystadegau" {
    datacenters = ["dc1"]
    type = "service"

    group "ystadegau" {
        count = 1

        network {
            port "api" { static = 5678 }
        }

        service {
            port = "api"
            tags = ["reproxy.enabled=true", "reproxy.server=api.chatha.dev", "reproxy.route=^/dub/(.*)"]
        }

        task "chwilwr" {
            driver = "exec"

            artifact {
                source = "https://bradley-chatha.s3.eu-west-2.amazonaws.com/artifacts/chwilwr_dist.zip"
                destination = "local/chwilwr"
            }

            config {
                command = "local/chwilwr/chwilwr"
            }

            resources {
                cpu = 100
                memory = 20
            }
        }

        task "gwyliwr" {
            driver = "exec"

            artifact {
                source = "https://bradley-chatha.s3.eu-west-2.amazonaws.com/artifacts/gwyliwr_dist.zip"
                destination = "local/gwyliwr"
            }

            config {
                command = "local/gwyliwr/gwyliwr"
            }

            resources {
                cpu = 100
                memory = 20
            }
        }
    }
}