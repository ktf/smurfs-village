## Smurfs Village board: tasks, comments, events and usage for the agent fleet.
#
#   nomad job run -output -var smurf_ref=<commit> smurf-board.nomad.hcl   # parse, submit nothing
#   nomad-rw job run -var smurf_ref=<commit> smurf-board.nomad.hcl        # register (human)
#
# While smurf is under heavy development there is no image of its own: the board
# task builds it at start-up in the stock Go image, from github.com/ktf/smurfs-village
# at the commit given as smurf_ref (a full hash, recorded in the job's version).
# Deploying a change is: push, then run with the new hash; rolling back is the
# old hash. deploy/Dockerfile is kept for when it settles into a docks image.
#
# One allocation, anywhere in the pool. The SQLite database lives on the group's
# ephemeral disk and Litestream streams it to s3://smurfs-village/board,
# so no node is special:
#   * a restart on the same node reuses the local copy (sticky ephemeral disk);
#   * a move to another node migrates it, or restores it from S3 if that fails;
#   * an unreachable S3 stops the task before it serves (entrypoint.sh), so an
#     empty board can never overwrite the good copy.
#
# Two writers on one S3 copy would corrupt it, so two allocations must never
# run at once. The board task runs under a Nomad variable lock (smurf lock, see
# entrypoint.sh): a replacement waits for it, and an allocation that cannot
# renew it stops at once. That needs a one-time ACL policy for the task's
# workload identity, deploy/smurf-board-lock.policy.hcl.
#
# S3 credentials follow queue-metrics.nomad: a security-proxy sidecar holds the
# bucket's keys (pushed from Vault by the bootstrap sidecar) and the board task
# only ever sees a rotating gate token.

variable "smurf_ref" {
  type        = string
  description = "Commit of github.com/ktf/smurfs-village to build and run (full hash; start.sh refuses anything else)"
}

job "smurf-board" {
  datacenters = ["meyrin"]
  type        = "service"

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  constraint {
    attribute = "${attr.driver.docker}"
    value     = "1"
  }

  group "board" {
    count = 1

    # A crash means "try again shortly", never "give up until a human
    # re-registers the job" (same reasoning as queue-metrics.nomad).
    restart {
      attempts = 3
      interval = "10m"
      delay    = "30s"
      mode     = "delay"
    }

    # Keeps downtime short: a restart or an in-place update finds the database
    # already on disk, and a reschedule copies it over when the old node is up.
    ephemeral_disk {
      size    = 1000
      sticky  = true
      migrate = true
    }

    # Belt and braces next to the lock: a node cut off from its servers stops
    # the board itself after a minute.
    disconnect {
      stop_on_client_after = "1m"
    }

    network {
      port "http" {}
    }

    service {
      name     = "smurf-board"
      port     = "http"
      provider = "consul"

      check {
        type     = "http"
        path     = "/healthz"
        interval = "15s"
        timeout  = "3s"
      }
    }

    # The node's grid host certificate: the proxy needs a client credential to
    # start, and the bootstrap task needs one for alivault.cern.ch.
    volume "grid-hostcert" {
      type      = "host"
      source    = "grid-hostcert"
      read_only = true
    }

    volume "grid-hostkey" {
      type      = "host"
      source    = "grid-hostkey"
      read_only = true
    }

    # ---- security-proxy sidecar ---------------------------------------------
    # One route: S3, and within it only the smurfs-village bucket's keypair.
    task "proxy" {
      driver = "docker"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      config {
        # Pinned by digest; repin together with queue-metrics.nomad.
        image        = "registry.cern.ch/alisw/security-proxy@sha256:be422fab6667f6d32e6863c49e095dd4489a049433d2f122b140806e52ab9d31"
        network_mode = "host"
        args = [
          "/usr/local/lib/security-proxy/venv/bin/security-proxy",
          "--config", "/local/config.json",
        ]
      }

      template {
        destination = "${NOMAD_ALLOC_DIR}/cern-ca-bundle.pem"
        data        = <<-EOC
        -----BEGIN CERTIFICATE-----
        MIIGqTCCBJGgAwIBAgIQAojDcLlcbrhBX0qrEka4mzANBgkqhkiG9w0BAQ0FADBK
        MQswCQYDVQQGEwJjaDENMAsGA1UEChMEQ0VSTjEsMCoGA1UEAxMjQ0VSTiBSb290
        IENlcnRpZmljYXRpb24gQXV0aG9yaXR5IDIwHhcNMTMwMzE5MTI1NTM2WhcNMzMw
        MzE5MTMwNTM0WjBKMQswCQYDVQQGEwJjaDENMAsGA1UEChMEQ0VSTjEsMCoGA1UE
        AxMjQ0VSTiBSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5IDIwggIiMA0GCSqG
        SIb3DQEBAQUAA4ICDwAwggIKAoICAQDxqYPFW2qVVi3Rw1NKlEf7x70xF+6a8uE/
        Tu4ZVQF/K2RXI95QLkYfKItZvy9Az3ib/VlUho5f8fBaqy4n70uwC7+qd3Aq1/xQ
        ysykPCbBBAsOSQQpTlhrMD2V5Ya9zrirphOhutddiqV96zBCyMM+Gz5uYv9u+cm4
        tg1EOmAMGh2UNxfTFNVmXKkk7eFTSC1+zgb28H6nd3xzV27sn9bfOfGh//ZPy5gm
        Qx0Oh/tc6WMreWzRZBQm5SJiK0QOzPv09p5WmdY2WxZoqNTFBDACQO7ysFOktc74
        fPVFX/lmt4jFNSZRIOvvaACI/qlEaAJTR4FHIY9uSMsV8DrtzhI1Ucyv3kqlQpbF
        jDouq44IryA/np4s/124bW+x8+n/v+at/AxPjvHBLiGhB+J38Z6KcJogoDnGzIXR
        S+YUr/vGz34jOmkRuDN5STuuAXzyCKFXaoAm0AwjTziIv3E0jxC1taw6FpKevnd1
        CLsTLAEUiEjzStFkDhd/Hpipc57zmMFY8VYet2wVqSFjnt2REWOVbZlbCiMHmSeD
        u5EuZLiU8xlkiaCfn4A5XZ6X0qprbgDviGJtwxzNvTg7Hn0ziW5/ELryfQXCwZJ+
        FVne8Zu8sbgy/sDkX+pyFuyB4XgiM0eMNkoexIXJaRdlMWDIL5ysiIXQKjhynAv5
        KLHbRjciVwIDAQABo4IBiTCCAYUwCwYDVR0PBAQDAgGGMA8GA1UdEwEB/wQFMAMB
        Af8wHQYDVR0OBBYEFPp7+96bDaPyUrds7VsPC6KmpvgEMBAGCSsGAQQBgjcVAQQD
        AgEAMIIBMgYDVR0gBIIBKTCCASUwggEhBgorBgEEAWAKBAEBMIIBETCBwgYIKwYB
        BQUHAgIwgbUegbIAQwBFAFIATgAgAFIAbwBvAHQAIABDAGUAcgB0AGkAZgBpAGMA
        YQB0AGkAbwBuACAAQQB1AHQAaABvAHIAaQB0AHkAIAAyACAAQwBlAHIAdABpAGYA
        aQBjAGEAdABlACAAUABvAGwAaQBjAHkAIABhAG4AZAAgAEMAZQByAHQAaQBmAGkA
        YwBhAHQAZQAgAFAAcgBhAGMAdABpAGMAZQAgAFMAdABhAHQAZQBtAGUAbgB0MEoG
        CCsGAQUFBwIBFj5odHRwOi8vY2FmaWxlcy5jZXJuLmNoL2NhZmlsZXMvY3AtY3Bz
        L2Nlcm4tcm9vdC1jYTItY3AtY3BzLnBkZjANBgkqhkiG9w0BAQ0FAAOCAgEAo0Px
        l4CZ6C6bDH+b6jV5uUO0NIHtvLuVgQLMdKVHtQ2UaxeIrWwD+Kz1FyJCHTRXrCvE
        OFOca9SEYK2XrbqZGvRKdDRsq+XYts6aCampXj5ahh6r4oQJ8U7aLVfziKTK13Gy
        dYFoAUeUrlNklICt3v2wWBaa1tg2oSlU2g4iCg9kYpRnIW3VKSrVsdVk2lUa4EXs
        nTEJ30OS7rqX3SdqZp8G+awtBEReh2XPhRgJ6w3xiScP/UdWYUam2LflCGX3RibB
        /DZhgGHRRoE4/D0kQMP2XTz6cClbNklECTlp0qZIbiaf350HbcDEFzYRSSIi0emv
        kRGcMgsi8yTTU87q8Cr4hETxAF3ZbSVNC0ZaTZ8RBbM9BXguhYzKkVBgG/cMpUjs
        B6tY2HMZbAZ3TKQRb/bRyUigM9DniKWeXkeL/0Nsno+XbcpAqLjtVIRwCg6jTLUi
        1NRsl3BP6C824dVaoI8Ry7m+o6O+mtocw4BMhHfTcoWCO8CWjT0ME67JzaAYa5eM
        +OqoWtgbgweBlfO0/3GMnVGMAmI4FlhH2oWKWQgWdgr0Wgh9K05VcxSpJ87/zjhb
        MQn/bEojWmp6eUppPaqNFcELvud41qoe6hLsOYQVUQ1sHi7n6ouhg4BAbwS2iyD2
        uiA6FHTCeLreFGUzs5osPKiz3GE5D6V9she9xIQ=
        -----END CERTIFICATE-----
        -----BEGIN CERTIFICATE-----
        MIIJnDCCB4SgAwIBAgIKYQQltAAAAAAACzANBgkqhkiG9w0BAQ0FADBKMQswCQYD
        VQQGEwJjaDENMAsGA1UEChMEQ0VSTjEsMCoGA1UEAxMjQ0VSTiBSb290IENlcnRp
        ZmljYXRpb24gQXV0aG9yaXR5IDIwHhcNMjIwMzI5MDgyNDIyWhcNMzIwMzI5MDgz
        NDIyWjBWMRIwEAYKCZImiZPyLGQBGRYCY2gxFDASBgoJkiaJk/IsZAEZFgRjZXJu
        MSowKAYDVQQDEyFDRVJOIEdyaWQgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwggIi
        MA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDS9Ypy1csm0aZA4/QnWe2oaiQI
        LqfeekV8kSSvOhW2peo5cLNIKbXATOo1l2iwIbCWV8SRU2TLKxHIL8fAOJud5n9K
        mEKBew7nzubl1wG93B4dY0KREdb3/QB/7OkG8ZZvLqrvQZVGT1CgJ+NFFUiJ315D
        FWkKctZv27LjQamzCxpX+gZSsmwZmSReY67cnm6P7z+/3xVNhwb+4Z+1Ww4vHhMc
        dh1Dsrkv9vXU01UN752QtQ6l56uQLYEB2+vaHB6IpyC9zAQ/33GulCq8Gbj7ykPd
        9AcRVBeJAErSK+oMHThtdLD7mhTkZivakaNe4O1EhPFH0rWwV45IFN7ipELA5qDx
        djdzo6JtLJQMaSV/TV+amEf2CaKlD0giqGhjfSNiOX5HCmpqV14kbl+7Qho6ykZy
        b1DGpf70yILnX+AUtdpd8lulTu1yg1Bg5cFQskUIk5+s4nsC1VpmeNxYaeFEcYZj
        Ph2mdD7zLo889MtF7kZv7+6J6p4NBL3fQ9Os8/h8XVlfDatzbpVH4jYKKAd4nwJb
        knJaKPE0LzLzVfJBwnDxqe8hb64gI8Frludp+jaOYzvMqlzAe9z4a9971iXIWaaG
        unbAoEkXj69y7MsvCjWXB7o9HdBaS9FL+ZtXTKCyXl+XLFseYQoQburKr+eTcRed
        KLJNj4tRF1799PO69wIDAQABo4IEdjCCBHIwEAYJKwYBBAGCNxUBBAMCAQEwIwYJ
        KwYBBAGCNxUCBBYEFGPCgXhtlBTXUVYziSFk8YWmsNHgMB0GA1UdDgQWBBSloP1m
        WP253Xrhsp2fo9HlUBiU5zCCAS4GA1UdIASCASUwggEhMIIBHQYKKwYBBAFgCgQB
        ATCCAQ0wgb4GCCsGAQUFBwICMIGxHoGuAEMARQBSAE4AIABHAHIAaQBkACAAQwBl
        AHIAdABpAGYAaQBjAGEAdABpAG8AbgAgAEEAdQB0AGgAbwByAGkAdAB5ACAAQwBl
        AHIAdABpAGYAaQBjAGEAdABlACAAUABvAGwAaQBjAHkAIABhAG4AZAAgAEMAZQBy
        AHQAaQBmAGkAYwBhAHQAZQAgAFAAcgBhAGMAdABpAGMAZQAgAFMAdABhAHQAZQBt
        AGUAbgB0MEoGCCsGAQUFBwIBFj5odHRwOi8vY2FmaWxlcy5jZXJuLmNoL2NhZmls
        ZXMvY3AtY3BzL2Nlcm4tZ3JpZC1jYS1jcC1jcHMucGRmADAZBgkrBgEEAYI3FAIE
        DB4KAFMAdQBiAEMAQTALBgNVHQ8EBAMCAYYwDwYDVR0TAQH/BAUwAwEB/zAfBgNV
        HSMEGDAWgBT6e/vemw2j8lK3bO1bDwuipqb4BDCCAUQGA1UdHwSCATswggE3MIIB
        M6CCAS+gggErhlJodHRwOi8vY2FmaWxlcy5jZXJuLmNoL2NhZmlsZXMvY3JsL0NF
        Uk4lMjBSb290JTIwQ2VydGlmaWNhdGlvbiUyMEF1dGhvcml0eSUyMDIuY3JshoHU
        bGRhcDovLy9DTj1DRVJOJTIwUm9vdCUyMENlcnRpZmljYXRpb24lMjBBdXRob3Jp
        dHklMjAyLENOPUNFUk5QS0lST09UMDIsQ049Q0RQLENOPVB1YmxpYyUyMEtleSUy
        MFNlcnZpY2VzLENOPVNlcnZpY2VzLENOPUNvbmZpZ3VyYXRpb24sREM9Y2VybixE
        Qz1jaD9jZXJ0aWZpY2F0ZVJldm9jYXRpb25MaXN0P2Jhc2U/b2JqZWN0Q2xhc3M9
        Y1JMRGlzdHJpYnV0aW9uUG9pbnQwggFEBggrBgEFBQcBAQSCATYwggEyMGcGCCsG
        AQUFBzAChltodHRwOi8vY2FmaWxlcy5jZXJuLmNoL2NhZmlsZXMvY2VydGlmaWNh
        dGVzL0NFUk4lMjBSb290JTIwQ2VydGlmaWNhdGlvbiUyMEF1dGhvcml0eSUyMDIu
        Y3J0MIHGBggrBgEFBQcwAoaBuWxkYXA6Ly8vQ049Q0VSTiUyMFJvb3QlMjBDZXJ0
        aWZpY2F0aW9uJTIwQXV0aG9yaXR5JTIwMixDTj1BSUEsQ049UHVibGljJTIwS2V5
        JTIwU2VydmljZXMsQ049U2VydmljZXMsQ049Q29uZmlndXJhdGlvbixEQz1jZXJu
        LERDPWNoP2NBQ2VydGlmaWNhdGU/YmFzZT9vYmplY3RDbGFzcz1jZXJ0aWZpY2F0
        aW9uQXV0aG9yaXR5MA0GCSqGSIb3DQEBDQUAA4ICAQAv56iMPo0VUkrHxPYLjfyW
        IL/TmYxxYldO8kCTKXyaRO4ZmwD6JjLaclTgSHz7gOKFL35ZF0Rv4nWk/ZJBl+dU
        1udgBjF/uKK0v0m+7iEIOG0HORCCQCDgayuiLomI5eQp8KTgHrswHWL+ESxa3Hdv
        vr7GBG/7EhrYwstm/tOJ8cKaeiooSxHw5Lgsqq229SxfO8fSyS8DAa5eUdWT/dVU
        RDR8lGQShx4R9JOHSDg0y6rE7V0cw/BO3NQuaxMunFXkQprtWneJfR4uugMOKk/v
        tMhQGCDB7o3CVhLGSb+76Tny+eSa2g+Zv17PGVfhnF9oynkCII+shX9TmOUsDEnS
        7MWES58YwnpBZrxdeJVPEzVVuYEZP4QsLrIL1ynFqBwFAnPU48Hs6s+kOI/9BFJz
        v+Fp/iw8BZSOclpJzA5rkW6yQ7LVfjFBV1CgyhO8GH5jhYBd5ZLvG8eLNm8Gpt+H
        n30awoaDoMuHcGS5B6NOZLfwE+suTxMw8pjHhKXx7RkSoeZy72PinlbWn1tWLiPa
        UMdkrb/WHOdMKaadQTDO/VyibBL49iJ8BAlERgIl9QaRDLjAIdD45rLdBe95HxSl
        zpZqsxuI09eJ8+iLFJhTDH2BODoEuqbn6PB/5z2d5zuG5sr85Vzn81ddapuUT9Ra
        /dB5eJQeFZ0WjtUOO3gS/A==
        -----END CERTIFICATE-----
        EOC
      }

      template {
        destination = "local/config.json"
        change_mode = "restart"
        data        = <<-EOH
        {
          "cert": {"ingest": "grid-cert"},
          "cafile": "/alloc/cern-ca-bundle.pem",
          "agent_socket":  "/alloc/security-proxy/agent/agent.sock",
          "ingest_socket": "/alloc/security-proxy/ingest/ingest.sock",
          "agent_socket_group": "securityproxy_clients",
          "secret_rotation_seconds": 86400,
          "routes": [
            {"name": "s3", "upstream": "https://s3.cern.ch",
             "s3": {"buckets": {"smurfs-village": {
               "access_key": {"ingest": "s3-access-smurfs-village"},
               "secret_key": {"ingest": "s3-secret-smurfs-village"}}}}}
          ]
        }
        EOH
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }

    # ---- slot provisioning --------------------------------------------------
    # A sidecar, so a restarted proxy is refilled (see queue-metrics.nomad).
    task "bootstrap" {
      driver = "docker"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      # Its own Vault role, usable only by this job (deploy/vault/setup.sh), that
      # reads smurfs-village/data/board and nothing else -- not the shared "nomad"
      # role, which reads all of kv/*.
      vault {
        role = "smurf-board"
      }

      user = "root"

      config {
        image        = "registry.cern.ch/alisw/security-proxy@sha256:be422fab6667f6d32e6863c49e095dd4489a049433d2f122b140806e52ab9d31"
        network_mode = "host"
        entrypoint   = ["/bin/sh"]
        args         = ["/local/bootstrap.sh"]
      }

      volume_mount {
        volume      = "grid-hostcert"
        destination = "/etc/grid-security/hostcert.pem"
        read_only   = true
      }

      volume_mount {
        volume      = "grid-hostkey"
        destination = "/etc/grid-security/hostkey.pem"
        read_only   = true
      }

      template {
        destination = "local/bootstrap.sh"
        perms       = "0755"
        data        = <<-EOH
        #!/bin/sh -e
        SOCK=/alloc/security-proxy/ingest/ingest.sock
        BIN=/usr/local/lib/security-proxy/venv/bin

        push_all () {
        cat /etc/grid-security/hostcert.pem /etc/grid-security/hostkey.pem \
          | "$BIN/security-proxy-push" grid-cert --socket "$SOCK" || return 1
        # A keypair of its own in the ALICE Release testing project, revocable
        # alone; the proxy signs with it for the smurfs-village bucket only.
        python3 /local/vault-field.py smurfs-village/data/board s3_access_key \
          | "$BIN/security-proxy-push" s3-access-smurfs-village --socket "$SOCK" || return 1
        python3 /local/vault-field.py smurfs-village/data/board s3_secret_key \
          | "$BIN/security-proxy-push" s3-secret-smurfs-village --socket "$SOCK" || return 1
        }

        # Reprovision whenever the proxy rebinds its ingest socket (inode change).
        provisioned=""
        while :; do
          current=$(stat -c %i "$SOCK" 2>/dev/null || echo "")
          if [ -n "$current" ] && [ "$current" != "$provisioned" ] && push_all; then
            provisioned=$current
            echo "provisioned grid-cert and the smurfs-village keypair"
          fi
          sleep 5
        done
        EOH
      }

      template {
        destination = "local/vault-field.py"
        perms       = "0755"
        data        = <<-EOP
        """Print one field of a Vault KV v2 secret. Never logs it."""
        import json, ssl, sys, urllib.request
        path, field = sys.argv[1], sys.argv[2]
        ctx = ssl.create_default_context()
        ctx.load_verify_locations("/alloc/cern-ca-bundle.pem")
        ctx.load_cert_chain("/etc/grid-security/hostcert.pem",
                            "/etc/grid-security/hostkey.pem")
        token = open("/secrets/vault_token").read().strip()
        req = urllib.request.Request("https://alivault.cern.ch/v1/" + path,
                                     headers={"X-Vault-Token": token})
        with urllib.request.urlopen(req, timeout=15, context=ctx) as r:
            data = json.load(r)["data"]["data"]
        value = data.get(field, "").strip()
        if not value:
            sys.exit("%s has no %s" % (path, field))
        sys.stdout.write(value)
        EOP
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }

    # ---- the board ----------------------------------------------------------
    task "board" {
      driver = "docker"

      config {
        # The stock Go image, pinned by digest: smurf is built at start-up (above).
        image        = "golang@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190"
        network_mode = "host"
        command      = "/bin/sh"
        args         = ["${NOMAD_TASK_DIR}/start.sh"]
      }

      # Unprivileged; group 8485 = securityproxy_clients, which owns the
      # sidecar's agent socket.
      user = "8486:8485"

      # The source at exactly smurf_ref. A commit hash, not a branch, so a
      # restart can never pick up something nobody deployed.
      artifact {
        source      = "git::https://github.com/ktf/smurfs-village.git"
        destination = "local/src"
        options {
          ref = var.smurf_ref
        }
      }

      # Litestream, checked against the release's published SHA-256.
      artifact {
        source      = "https://github.com/benbjohnson/litestream/releases/download/v0.5.17/litestream-0.5.17-linux-x86_64.tar.gz"
        destination = "local/litestream"
        options {
          checksum = "sha256:cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d"
        }
      }

      template {
        destination = "local/start.sh"
        perms       = "0755"
        data        = <<-EOS
        #!/bin/sh
        # Build smurf from the fetched source, then hand over to entrypoint.sh.
        # Go's caches live on the sticky ephemeral disk, so a restart on the same
        # node rebuilds in seconds; only a fresh node compiles from scratch.
        set -eu
        # A full commit hash only: a branch would let a restart run whatever the
        # branch says by then. (Checked here because Nomad's HCL has no regex.)
        ref="${var.smurf_ref}"
        case "$ref" in *[!0-9a-f]*|"") ref=bad ;; esac
        if [ $${#ref} -ne 40 ]; then
          echo "smurf_ref must be a full 40-character commit hash, got: ${var.smurf_ref}" >&2
          exit 1
        fi
        data="$${NOMAD_ALLOC_DIR}/data"
        mkdir -p "$data/bin" "$data/go"
        export HOME="$data/go" GOCACHE="$data/go/cache" GOMODCACHE="$data/go/mod"
        export CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local
        cd "$${NOMAD_TASK_DIR}/src/smurf"
        go build -o "$data/bin/smurf" ./cmd/smurf
        echo "built smurf at ${var.smurf_ref}"

        export PATH="$data/bin:$${NOMAD_TASK_DIR}/litestream:$PATH"
        export LITESTREAM_CONFIG="$${NOMAD_TASK_DIR}/src/smurf/deploy/litestream.yml"
        export SMURF_STATE_DIR="$data"
        exec sh "$${NOMAD_TASK_DIR}/src/smurf/deploy/entrypoint.sh"
        EOS
      }

      # Time for Litestream to ship the last second of writes on a stop.
      kill_timeout = "30s"

      # NOMAD_TOKEN for the Task API socket, used by smurf lock.
      identity {
        env = true
      }

      resources {
        cpu    = 1000 # the go build at start-up; the board itself needs far less
        memory = 1024
      }
    }
  }
}
