f(){
  prod_tag=$1
  prod_deployment_version=${prod_tag/promote-prod-/}
  echo  $prod_deployment_version
}

f $1