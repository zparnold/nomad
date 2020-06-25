# lifecycle hook test job for batch jobs. touches, removes, and tests
# for the existence of files to assert the order of running tasks.
# all tasks should exit 0 and the alloc dir should contain the following
# files: ./init-ran, ./main-ran, ./cleanup-run

job "batch-lifecycle" {

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
if [ -f ${NOMAD_ALLOC_DIR}/cleanup-running ]; then exit 8; fi
rm ${NOMAD_ALLOC_DIR}/init-running
EOT

        destination = "local/prestart.sh"

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
sleep 1
touch ${NOMAD_ALLOC_DIR}/main-ran
touch ${NOMAD_ALLOC_DIR}/main-running
if [ ! -f ${NOMAD_ALLOC_DIR}/init-ran ]; then exit 9; fi
if [ -f ${NOMAD_ALLOC_DIR}/init-running ]; then exit 10; fi
if [ -f ${NOMAD_ALLOC_DIR}/cleanup-running ]; then exit 11; fi
rm ${NOMAD_ALLOC_DIR}/main-running
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
sleep 1
touch ${NOMAD_ALLOC_DIR}/cleanup-ran
touch ${NOMAD_ALLOC_DIR}/cleanup-running
if [ ! -f ${NOMAD_ALLOC_DIR}/init-ran ]; then exit 12; fi
if [ ! -f ${NOMAD_ALLOC_DIR}/main-ran ]; then exit 13; fi
if [ -f ${NOMAD_ALLOC_DIR}/init-running ]; then exit 14; fi
if [ -f ${NOMAD_ALLOC_DIR}/main-running ]; then exit 15; fi
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
