# lifecycle hook test job for service jobs. touches, removes, and tests
# for the existence of files to assert the order of running tasks.
# after stopping, the alloc dir should contain the following files:
# files: ./init-ran, ./sidecar-ran, ./main-ran, ./cleanup-run but not
# the ./main-running, ./sidecar-running, or ./cleanup-running files

job "service-lifecycle" {

  datacenters = ["dc1"]

  type = "batch"

  group "test" {

    task "init" {

      lifecycle {
        hook = "prestart"
      }

      driver = "docker"

      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["local/prestart.sh"]
      }

      template {
        data = <<EOT
#!/bin/sh
sleep 1
touch ${NOMAD_ALLOC_DIR}/init-ran
touch ${NOMAD_ALLOC_DIR}/init-running
if [ -f ${NOMAD_ALLOC_DIR}/main ]; then exit 7; fi
if [ -f ${NOMAD_ALLOC_DIR}/cleanup ]; then exit 8; fi
rm ${NOMAD_ALLOC_DIR}/init-running
EOT

        destination = "local/prestart.sh"

      }

      resources {
        cpu    = 64
        memory = 64
      }
    }

    task "sidecar" {

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      driver = "docker"

      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["local/sidecar.sh"]
      }

      template {
        data = <<EOT
#!/bin/sh
touch ${NOMAD_ALLOC_DIR}/sidecar-ran
touch ${NOMAD_ALLOC_DIR}/sidecar-running
sleep 5
if [ ! -f ${NOMAD_ALLOC_DIR}/main-running ]; then exit 9; fi
if [ -f ${NOMAD_ALLOC_DIR}/cleanup-running ]; then exit 10; fi
sleep 300
EOT

        destination = "local/sidecar.sh"

      }

      resources {
        cpu    = 64
        memory = 64
      }
    }

    task "main" {

      driver = "docker"

      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["local/main.sh"]
      }

      template {
        data = <<EOT
#!/bin/sh
touch ${NOMAD_ALLOC_DIR}/main-ran
touch ${NOMAD_ALLOC_DIR}/main-running
sleep 5
if [ ! -f ${NOMAD_ALLOC_DIR}/init-ran ]; then exit 11; fi
if [ -f ${NOMAD_ALLOC_DIR}/init-running ]; then exit 12; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/sidecar-ran ]; then exit 13; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/sidecar-running ]; then exit 14; fi
if [ -f ${NOMAD_ALLOC_DIR}/cleanup-running ]; then exit 15; fi
sleep 300
EOT

        destination = "local/main.sh"
      }

      resources {
        cpu    = 64
        memory = 64
      }
    }


    task "cleanup" {

      lifecycle {
        hook = "poststop"
      }

      driver = "docker"

      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["local/cleanup.sh"]
      }

      template {
        data = <<EOT
#!/bin/sh
touch ${NOMAD_ALLOC_DIR}/cleanup-ran
touch ${NOMAD_ALLOC_DIR}/cleanup-running
if [ ! -f ${NOMAD_ALLOC_DIR}/init-ran ]; then exit 16; fi
if [ -f ${NOMAD_ALLOC_DIR}/init-running ]; then exit 17; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/sidecar-ran ]; then exit 18; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/sidecar-running ]; then exit 19; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/main-ran ]; then exit 20; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/main-running ]; then exit 20; fi
rm ${NOMAD_ALLOC_DIR}/sidecar-running
rm ${NOMAD_ALLOC_DIR}/main-running
rm ${NOMAD_ALLOC_DIR}/cleanup-running
EOT

        destination = "local/cleanup.sh"
      }

      resources {
        cpu    = 64
        memory = 64
      }
    }

  }
}
