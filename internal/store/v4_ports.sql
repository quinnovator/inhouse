-- Loopback ports a revision publishes besides its ingress, by key: routed
-- HTTP ports (http/7880) and raw tailnet ports (udp/7882). Released with
-- host_port, once the revision's pod is gone.
ALTER TABLE revisions ADD COLUMN host_ports TEXT NOT NULL DEFAULT '{}';
