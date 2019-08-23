# Using the S3 Gateway

Pachyderm Enterprise includes an S3 gateway that enables you to interact
with PFS storage through an HTTP application programming interface (API)
that imitates the Amazon S3 Storage API. Therefore, with Pachyderm S3
gateway, you can enable tools or applications that are designed
to work with object stores, such as [MinIO™](https://min.io/) and
[Boto3](https://boto3.amazonaws.com/v1/documentation/api/latest/index.html),
to interact with Pachyderm.

When you deploy `pachd`, the S3 gateway starts automatically. However, the
S3 gateway is an enterprise feature that is only available to paid customers or
during the free trial evaluation. You can confirm that the S3 gateway is running
by pointing your browser to the following URL:

```bash
http://localhost:30600/
```

Through the S3 gateway, you can only interact with the `HEAD` commits of
your Pachyderm branches that do not require authorization. If you need
to have a more granular access to branches and commits, use the PFS
gRPC Remote Procedure Call (gRPC) interface instead.

You can use any S3 compliant client, such as [MinIO](https://docs.min.io/docs/minio-client-complete-guide),
[AWS CLI S3](https://docs.aws.amazon.com/cli/latest/reference/s3/index.html), or
[S3cmd](https://s3tools.org/usage) to interact with the Pachyderm S3 gateway.

## Viewing the List of S3 Buckets

The S3 gateway presents each branch from every Pachyderm repository as
an S3 bucket.
For example, if you have a `master` branch in the `images` repository,
an S3 tool sees `images@master` as the `master.images` S3 bucket.

To view the list of S3 buckets, complete the following steps:

1. If you have not done so already, forward Pachyderm ports to
enable access to the Pachyderm UI and the S3 gateway:

   ```bash
   pachctl port-forward
   ```

1. Point your browser to `http://<cluster-ip>:30600/`. A list of
S3 buckets appears. Example:

   ![S3 buckets](../_images/s_list_of_buckets.png)

1. Alternatively, you can use `curl`:

   ```
   $ curl http://localhost:30600
   <ListAllMyBucketsResult><Owner><ID>00000000000000000000000000000000</ID><DisplayName>pachyderm</DisplayName></Owner><Buckets><Bucket><Name>master. train</Name><CreationDate>2019-07-12T22:09:50.274391271Z</CreationDate></Bucket><Bucket><Name>master.pre_process</Name><CreationDate>2019-07-12T21:   58:50.930608352z</CreationDate></Bucket><Bucket><Name>master.split</Name><CreationDate>2019-07-12T21:58:09.074523275Z</CreationDate></                Bucket><Bucket><Name>stats.split</Name><CreationDate>2019-07-12T21:58:09.074523275Z</CreationDate></Bucket><Bucket><Name>master.raw_data</Name><CreationDate>2019-07-12T21:36:27.975670319Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>
   ```

   You can specify `localhost` instead of the cluster IP
   address to access the Pachyderm Dashboard and the S3 gateway.
   For this to work, enable
   port forwarding by running the `pachctl port-forward` command.

   However, Pachyderm does not recommend to heavily rely on port forwarding.
   Because Kubernetes' port forwarder incurs overhead, it might
   not recover well from broken connections. Therefore, using
   Pachyderm contexts or connecting to the S3 gateway directly
   through the cluster IP address are more reliable and preferred
   options.

## Configure the S3 client

Before you can work with the S3 gateway, configure your S3 client
to access Pachyderm. Complete the steps in one of the sections below that
correspond to your S3 client.

### Configure MinIO

If you are using AWS CLI or S3cmd, skip this section.

To install and configure MinIO, complete the following steps:

1. Install the MinIO client on your platform as
described on the [MinIO download page](https://min.io/download#/macos).
1. Verify that MinIO components are successfully installed by running
the following command:

   ```bash
   $ minio version
   $ mc version
   Version: 2019-07-11T19:31:28Z
   Release-tag: RELEASE.2019-07-11T19-31-28Z
   Commit-id: 31e5ac02bdbdbaf20a87683925041f406307cfb9
   ```

1. Set up the MinIO configuration file to use the `30600` port for your host:

   ```bash
   vi ~/.mc/config.json
   ```

   You should see a configuration similar to the following:

   * For a minikube deployment, verify the
   `local` host configuration:

     ```bash
     "local": {
               "url": "http://localhost:30600",
               "accessKey": "",
               "secretKey": "",
               "api": "S3v4",
               "lookup": "auto"
           },
     ```

### Configure the AWS CLI

If you are using the MinIO client or S3cmd, skip this section.

If you have not done so already, you need to install and
configure the AWS CLI client on your machine. You need to
provide the AWS Access Key ID and the AWS Secret Access Keys
for the account that has access to the S3 bucket that you want
to use with Pachyderm.
To configure the AWS CLI, complete the following steps:

1. Install the AWS CLI for your operating system as described
in the [AWS documentation](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-install.html).

1. Verify that the AWS CLI is installed:

   ```bash
   $ aws --version aws-cli/1.16.204 Python/2.7.16 Darwin/17.7.0 botocore/1.12.194
   ```

1. Configure AWS CLI:

   ```bash
   $ aws configure
   AWS Access Key ID:
   AWS Secret Access Key:
   Default region name:
   Default output format [None]:
   ```

### Configure S3cmd

If you are using AWS CLI or MinIO, skip this section.

S3cmd is an open-source command line client that enables you
to access S3 object store buckets. To configure S3cmd, complete
the following steps:

1. If you do not have S3cmd installed on your machine, install
it as described in the [S3cmd documentation](https://s3tools.org/download).
For example, in macOS, run:

   ```bash
   $ brew install s3cmd
   ```

1. Verify that S3cmd is installed:

   ```bash
   $ s3cmd --version
   s3cmd version 2.0.2
   ```

1. Configure S3cmd to use Pachyderm:

   ```bash
   $ s3cmd --configure
     ...
   ```

1. Fill all fields and specify the following settings for Pachyderm.

   **Example:**

   ```bash

   New settings:
     Access Key: ""
     Secret Key: ""
     Default Region: US
     S3 Endpoint: localhost:30600
     DNS-style bucket+hostname:port template for accessing a bucket: localhost:30600/%(bucket)
     Encryption password:
     Path to GPG program: /usr/local/bin/gpg
     Use HTTPS protocol: False
     HTTP Proxy server name:
     HTTP Proxy server port: 0
   ```

## Examples of Command-Line Operations

The Pachyderm S3 gateway supports the following operations:

* Create buckets: Creates a repo and branch.
* Delete buckets: Deletes a branch or a repo with all branches.
* List buckets: Lists all branches on all repos as S3 buckets.
* Write objects: Atomically overwrites a file on the HEAD of a branch.
* Remove objects: Atomically removes a file on the HEAD of a branch.
* List objects: Lists the files in the HEAD of a branch.
* Get objects: Gets file contents on the HEAD of a branch.

You can use any S3 compatible tool, such as MinIO, AWS CLI, or
S3cmd to interact with the Pachyderm S3 gateway.

### List Filesystem Objects

If you have configured your S3 client correctly, you should be
able to see the list of filesystem objects in your Pachyderm
repository by running an S3 client `ls` command.

To list filesystem objects, complete the following steps:

1. Verify that your S3 client can access all of your Pachyderm repositories:

   * If you are using MinIO, type:

     ```bash
     $ mc ls local
     [2019-07-12 15:09:50 PDT]      0B master.train/
     [2019-07-12 14:58:50 PDT]      0B master.pre_process/
     [2019-07-12 14:58:09 PDT]      0B master.split/
     [2019-07-12 14:58:09 PDT]      0B stats.split/
     [2019-07-12 14:36:27 PDT]      0B master.raw_data/
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600 s3 ls
     2019-07-12 15:09:50 master.train
     2019-07-12 14:58:50 master.pre_process
     2019-07-12 14:58:09 master.split
     2019-07-12 14:58:09 stats.split
     2019-07-12 14:36:27 master.raw_data
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd ls
     2019-07-12 15:09 master.train
     2019-07-12 14:58 master.pre_process
     2019-07-12 14:58 master.split
     2019-07-12 14:58 stats.split
     2019-07-12 14:36 master.raw_data
     ```

1. List the contents of a repository:

   * If you are using MinIO, type:

     ```bash
     $ mc ls local/master.raw_data
     [2019-07-19 12:11:37 PDT]  2.6MiB github_issues_medium.csv
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 ls s3://master.raw_data
     2019-07-26 11:22:23    2685061 github_issues_medium.csv
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd ls s3://master.raw_data/
     2019-07-26 11:22 2685061 s3://master.raw_data/github_issues_medium.csv
     ```
### Create an S3 Bucket

You can create an S3 bucket in Pachyderm by using the AWS CLI or
the MinIO client commands.
The S3 bucket that you create is a branch in a repository
in Pachyderm.

To create an S3 bucket, complete the following steps:

1. Use the `mb <host/branch.repo>` command to create a new
S3 bucket, which is a repository with a branch in Pachyderm.

   * If you are using MinIO, type:

     ```bash
     $ mc mb local/master.test
     Bucket created successfully `local/master.test`.
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 mb s3://master.test
     make_bucket: master.test
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd mb s3://master.test
     ```

   This command creates the `test` repository with the `master` branch.

1. Verify that the S3 bucket has been successfully created:

   * If you are using MinIO, type:

     ```bash
     $ mc ls local
     [2019-07-18 13:32:44 PDT]      0B master.test/
     [2019-07-12 15:09:50 PDT]      0B master.train/
     [2019-07-12 14:58:50 PDT]      0B master.pre_process/
     [2019-07-12 14:58:09 PDT]      0B master.split/
     [2019-07-12 14:58:09 PDT]      0B stats.split/
     [2019-07-12 14:36:27 PDT]      0B master.raw_data/
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 ls
     2019-07-26 11:35:28 master.test
     2019-07-12 14:58:50 master.pre_process
     2019-07-12 14:58:09 master.split
     2019-07-12 14:58:09 stats.split
     2019-07-12 14:36:27 master.raw_data
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd ls
     2019-07-26 11:35 master.test
     2019-07-12 14:58 master.pre_process
     2019-07-12 14:58 master.split
     2019-07-12 14:58 stats.split
     2019-07-12 14:36 master.raw_data
     ```

   * You can also use the `pachctl list repo` command to view the
   list of repositories:

     ```bash
     $ pachctl list repo
     NAME               CREATED                    SIZE (MASTER)
     test               About an hour ago          0B
     train              6 days ago                 68.57MiB
     pre_process        6 days ago                 1.18MiB
     split              6 days ago                 1.019MiB
     raw_data           6 days ago                 2.561MiB
     ```

     You should see the newly created repository in this list.

### Delete an S3 Bucket

You can delete an S3 bucket in Pachyderm from the AWS CLI or
MinIO client by running the following command:

   * If you are using MinIO, type:

     ```bash
     $ mc rb local/master.test
     Removed `local/master.test` successfully.
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 rb s3://master.test
     remove_bucket: master.test
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd rb s3://master.test
     ```

### Upload and Download File Objects

For input repositories at the top of your DAG, you can both add files
to and download files from the repository.

When you add files, Pachyderm automatically overwrites the previous
version of the file if it already exists.
Uploading new files is not supported for output repositories,
these are the repositories that are the output of a pipeline.

If you try to upload
a file to an output repository, you get an error message:

```bash
Failed to copy `github_issues_medium.csv`. cannot start a commit on an output
branch
```

Not all the repositories that you see in the results of the `ls` command are
input repositories that can be written to. Some of them might be read-only
output repos. Check your pipeline specification to verify which
repositories are the input repos.

To add a file to a repository, complete the following steps:

1. Run the `cp` command for your S3 client:

   * If you are using MinIO, type:

     ```bash
     $ mc cp test.csv local/master.raw_data/test.csv
     test.csv:                  62 B / 62 B  ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  100.00% 206 B/s 0s
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 cp test.csv s3://master.raw_data
     upload: ./test.csv to s3://master.raw_data/test.csv
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd cp test.csv s3://master.raw_data
     ```

   These commands add the `test.csv` file to the `master` branch in
   the `raw_data` repository. `raw_data` is an input repository.

1. Check that the file was added:

   * If you are using MinIO, type:

     ```bash
     $ mc ls local/master.raw_data
     [2019-07-19 12:11:37 PDT]  2.6MiB github_issues_medium.csv
     [2019-07-19 12:11:37 PDT]     62B test.csv
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 ls s3://master.raw_data/
     2019-07-19 12:11:37  2685061 github_issues_medium.csv
     2019-07-19 12:11:37       62 test.csv
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd ls s3://master.raw_data/
     2019-07-19 12:11  2685061 github_issues_medium.csv
     2019-07-19 12:11       62 test.csv
     ```

1. Download a file from MinIO to the
current directory by running the following commands:

   * If you are using MinIO, type:

     ```
     $ mc cp local/master.raw_data/github_issues_medium.csv .
     ...hub_issues_medium.csv:  2.56 MiB / 2.56 MiB  ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓ 100.00% 1.26 MiB/s 2s
     ```

   * If you are using AWS, type:

     ```
     $ aws --endpoint-url http://localhost:30600/ s3 cp s3://master.raw_data/test.csv .
     download: s3://master.raw_data/test.csv to ./test.csv
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd cp s3://master.raw_data/test.csv .
     ```

### Remove a File Object

You can delete a file in the `HEAD` of a Pachyderm branch by using the
MinIO command-line interface:

1. List the files in the input repository:

   * If you are using MinIO, type:

     ```bash
     $ mc ls local/master.raw_data/
     [2019-07-19 12:11:37 PDT]  2.6MiB github_issues_medium.csv
     [2019-07-19 12:11:37 PDT]     62B test.csv
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 ls s3://master.raw_data
     2019-07-19 12:11:37    2685061 github_issues_medium.csv
     2019-07-19 12:11:37         62 test.csv
     ```

   * If you are using S3cmd, type:

      ```bash
      $ s3cmd ls s3://master.raw_data
      2019-07-19 12:11    2685061 github_issues_medium.csv
      2019-07-19 12:11         62 test.csv
      ```

1. Delete a file from a repository. Example:

   <!--- AFAIU, this is supposed to work, but it does not.-->

   * If you are using MinIO, type:

     ```bash
     $ mc rm local/master.raw_data/test.csv
     Removing `local/master.raw_data/test.csv`.
     ```

   * If you are using AWS, type:

     ```bash
     $ aws --endpoint-url http://localhost:30600/ s3 rm s3://master.raw_data/test.csv
     delete: s3://master.raw_data/test.csv
     ```

   * If you are using S3cmd, type:

     ```bash
     $ s3cmd rm s3://master.raw_data/test.csv
     ```

## Unsupported operations

Some of the S3 functionalities are not yet supported by Pachyderm..
If you run any of these operations, Pachyderm returns a standard
`NotImplemented` error.

The S3 Gateway does not support the following S3 operations:

* Accelerate
* Analytics
* Object copying. PFS supports this functionality through gRPC.
* CORS configuration
* Encryption
* HTML form uploads
* Inventory
* Legal holds
* Lifecycles
* Logging
* Metrics
* Multipart uploads. See writing object documentation above for a workaround.
* Notifications
* Object locks
* Payment requests
* Policies
* Public access blocks
* Regions
* Replication
* Retention policies
* Tagging
* Torrents
* Website configuration

In addition, the Pachyderm S3 gateway has the following limitations:

* No support for authentication or ACLs.
* As per PFS rules, you cannot write to an output repo. At the
moment, Pachyderm returns a 500 error code.
