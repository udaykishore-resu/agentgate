# Local Alertmanager secret files

Placeholder values so the `*_file` references in
`deploy/prometheus/alertmanager.yml` resolve when the `alerting` compose
profile is running. They are intentionally invalid: the local stack must never
be able to page a real on-call rota or open a real ServiceNow record.

In every deployed environment these paths are backed by a
`SecretProviderClass` (Azure Key Vault CSI) or an `ExternalSecret`
(AWS Secrets Manager), mounted read-only at `/etc/alertmanager/secrets`.
