resource "aws_iam_openid_connect_provider" "github" {
  count = var.github_repo != "" ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]

  # SHA1 thumbprint of GitHub's OIDC TLS certificate.
  # AWS now validates GitHub tokens against the JWKS endpoint directly, so this
  # is effectively a formality, but at least one thumbprint is required by the API.
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]
}

# GitHub's immutable OIDC subject prefix (repo setting use_immutable_subject):
# repo:<owner>@<owner_id>/<name>@<repo_id>. A renamed or delete-and-recreated
# repo gets a new id, so a name squatter cannot match. Every trust subject in
# role.tf is built from it; never reintroduce the name-only form.
locals {
  github_subject_prefix = var.github_repo == "" ? "" : "repo:${split("/", var.github_repo)[0]}@${var.github_owner_id}/${split("/", var.github_repo)[1]}@${var.github_repo_id}"
}
