module "aws-app" {
  source = "git@github.com:TimeIncOSS/ape-dev-tf-aws-app.git"

  aws_access_key = ""
  aws_secret_key = ""

  aws_region = "${terraform_remote_state.shared-service.output.aws_region}"
  aws_jumpbox_sg_id = "${terraform_remote_state.shared-service.output.aws_jumpbox_sg_id}"
  aws_vpc_id = "${terraform_remote_state.shared-service.output.aws_vpc_id}"
  division_id = "{{.TeamName}}"
  env_id = "${terraform_remote_state.shared-service.output.env_name}"
  app_id = "{{.AppName}}"
  app_stack_id = ""
  aws_hosted_zone_id = "${terraform_remote_state.shared-service.output.aws_hosted_zone_id}"
  dns_domain = "{{.AppName}}"
}
{{ $slice := mkSlice "bucket1" "bucket2" "bucket3" }}
{{ range $index, $path := $slice }}
resource "aws_s3_bucket" "{{$index}}_pixel_bucket" {
bucket = "{{$path}}-pixel-bucket"
acl = "private"
}
{{ end }}
