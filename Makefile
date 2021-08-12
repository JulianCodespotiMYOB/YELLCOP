
VERSION		!= git describe --tags --always
PROJECT_SCOPE	= yellcop
PROJECT_NAME	= lambda
AWS_REGION	= ap-southeast-2
STACK_NAME	= $(PROJECT_SCOPE)-$(PROJECT_NAME)
ENVIRONMENT	= lab
BINARY		= yellcop

deploy: $(BINARY).zip package.yaml
	aws cloudformation deploy \
		--region $(AWS_REGION) \
		--template-file package.yaml \
		--stack-name $(STACK_NAME) \
		--tags project=$(PROJECT_SCOPE) environment=$(ENVIRONMENT) system=$(PROJECT_NAME) \
		--capabilities CAPABILITY_IAM CAPABILITY_NAMED_IAM \
		--parameter-override Revision=$(VERSION) Environment=$(ENVIRONMENT) \
		--no-fail-on-empty-changeset
	rm package.yaml

package.yaml: template.yaml
	aws cloudformation package \
		--region $(AWS_REGION) \
		--template-file template.yaml \
		--output-template-file package.yaml \
		--s3-prefix $(PROJECT_SCOPE)/$(PROJECT_NAME) \
		--s3-bucket kepler-deployment-$(ENVIRONMENT)

$(BINARY).zip: $(BINARY)
	zip $(BINARY).zip $(BINARY)

$(BINARY): main.go go.mod
	GOOS=linux go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY)

test:
	go vet
	go test

clean:
	rm $(BINARY)
	rm $(BINARY).zip
