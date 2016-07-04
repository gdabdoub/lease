package lease

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
)

const (
	// Table schema
	LeaseKeyKey     = "leaseKey"
	LeaseOwnerKey   = "leaseOwner"
	LeaseCounterKey = "leaseCounter"

	// AWS exception
	AlreadyExist = "ResourceInUseException"

	// Max number of retries
	maxScanRetries   = 3
	maxCreateRetries = 3
	maxUpdateRetries = 2
	maxDeleteRetries = 2
)

// Manager wrap the basic operations for leases.
type Manager interface {
	// Creates the table that will store leases if it's not already exists.
	CreateLeaseTable() error

	// List all leases(objects) in table.
	ListLeases() ([]*Lease, error)

	// Renew a lease
	RenewLease(*Lease) error

	// Take a lease
	TakeLease(*Lease) error

	// Evict a lease
	EvictLease(*Lease) error

	// Delete a lease
	DeleteLease(*Lease) error

	// Create a lease
	CreateLease(*Lease) error
}

// LeaseManager is the default implemntation of Manager
// that uses DynamoDB.
type LeaseManager struct {
	*Config
}

// CreateLeaseTable creates the table that will store the leases. succeeds
// if it's  already exists.
func (l *LeaseManager) CreateLeaseTable() (err error) {
	for l.Backoff.Attempt() < maxCreateRetries {
		_, err = l.Client.CreateTable(&dynamodb.CreateTableInput{
			TableName: aws.String(l.LeaseTable),
			AttributeDefinitions: []*dynamodb.AttributeDefinition{
				{
					AttributeName: aws.String(LeaseKeyKey),
					AttributeType: aws.String(dynamodb.ScalarAttributeTypeS),
				},
			},
			KeySchema: []*dynamodb.KeySchemaElement{
				{
					AttributeName: aws.String(LeaseKeyKey),
					KeyType:       aws.String("HASH"),
				},
			},
			ProvisionedThroughput: &dynamodb.ProvisionedThroughput{
				ReadCapacityUnits:  aws.Int64(int64(l.LeaseTableReadCap)),
				WriteCapacityUnits: aws.Int64(int64(l.LeaseTableWriteCap)),
			},
		})

		if err == nil {
			break
		}

		if awsErr, ok := err.(awserr.RequestFailure); ok && awsErr.Code() == AlreadyExist {
			err = nil
			break
		}

		backoff := l.Backoff.Duration()

		l.Logger.WithFields(logrus.Fields{
			"backoff": backoff,
			"attempt": int(l.Backoff.Attempt()),
		}).Warnf("Worker %s failed to create table", l.WorkerId)

		time.Sleep(backoff)
	}
	l.Backoff.Reset()
	return
}

// Renew a lease by incrementing the lease counter.
// Conditional on the leaseCounter in DynamoDB matching the leaseCounter of the input
// Mutates the leaseCounter of the passed-in lease object after updating the record in DynamoDB.
func (l *LeaseManager) RenewLease(lease *Lease) (err error) {
	clease := *lease
	clease.Counter++
	if err = l.updateLease(clease, *lease); err == nil {
		lease.Counter = clease.Counter
	}
	return
}

// Evict the current owner of lease by setting owner to null
// Conditional on the owner in DynamoDB matching the owner of the input.
// Mutates the lease owner of the passed-in lease object after updating the record in DynamoDB.
func (l *LeaseManager) EvictLease(lease *Lease) (err error) {
	clease := *lease
	clease.Owner = "NULL"
	if err = l.updateLease(clease, *lease); err == nil {
		lease.Owner = clease.Owner
	}
	return
}

// Take a lease by incrementing its leaseCounter and setting its owner field.
// Conditional on the leaseCounter in DynamoDB matching the leaseCounter of the input
// Mutates the lease counter and owner of the passed-in lease object after updating the record in DynamoDB.
func (l *LeaseManager) TakeLease(lease *Lease) (err error) {
	clease := *lease
	clease.Counter++
	clease.Owner = l.WorkerId
	if err = l.updateLease(clease, *lease); err == nil {
		lease.Owner = clease.Owner
		lease.Counter = clease.Counter
	}
	return
}

// UpdateLease gets a lease and update it in the leasing table.
func (l *LeaseManager) updateLease(updateLease, condLease Lease) (err error) {
	updateInput := &dynamodb.UpdateItemInput{
		TableName: aws.String(l.LeaseTable),
		Key: map[string]*dynamodb.AttributeValue{
			LeaseKeyKey: {
				S: aws.String(updateLease.Key),
			},
		},
		ReturnValues: aws.String(dynamodb.ReturnValueAllNew),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":owner": {
				S: aws.String(updateLease.Owner),
			},
			":count": {
				N: aws.String(strconv.Itoa(updateLease.Counter)),
			},
		},
		UpdateExpression: aws.String(fmt.Sprintf(
			"SET %s = :owner, %s = :count",
			LeaseOwnerKey,
			LeaseCounterKey,
		)),
	}

	// add conditions only to veteran leases
	var condExp string
	var attrExp = make(map[string]*string)
	if condLease.Counter > 0 {
		updateInput.ExpressionAttributeValues[":condCounter"] = &dynamodb.AttributeValue{
			N: aws.String(strconv.Itoa(condLease.Counter)),
		}
		attrExp["#counter"] = aws.String(LeaseCounterKey)
		condExp = ":condCounter = #counter"
	}
	if condLease.Owner != "" {
		updateInput.ExpressionAttributeValues[":condOwner"] = &dynamodb.AttributeValue{
			S: aws.String(condLease.Owner),
		}
		attrExp["#owner"] = aws.String(LeaseOwnerKey)
		if condExp != "" {
			condExp += " AND "
		}
		condExp += ":condOwner = #owner"
	}
	if condExp != "" {
		updateInput.ExpressionAttributeNames = attrExp
		updateInput.ConditionExpression = aws.String(condExp)
	}

	for l.Backoff.Attempt() < maxUpdateRetries {
		_, err = l.Client.UpdateItem(updateInput)

		if err == nil {
			break
		}

		backoff := l.Backoff.Duration()

		l.Logger.WithFields(logrus.Fields{
			"backoff": backoff,
			"attempt": int(l.Backoff.Attempt()),
		}).Warnf("Worker %s failed to update lease", l.WorkerId)

		time.Sleep(backoff)
	}

	l.Backoff.Reset()
	return
}

// ListLeasses returns all the lease units stored in the table.
func (l *LeaseManager) ListLeases() (list []*Lease, err error) {
	var res *dynamodb.ScanOutput
	for l.Backoff.Attempt() < maxScanRetries {
		res, err = l.Client.Scan(&dynamodb.ScanInput{
			TableName: aws.String(l.LeaseTable),
		})
		if err != nil {
			backoff := l.Backoff.Duration()

			l.Logger.WithFields(logrus.Fields{
				"backoff": backoff,
				"attempt": int(l.Backoff.Attempt()),
			}).Warnf("Worker %s failed to scan leases table", l.WorkerId)

			time.Sleep(backoff)
			continue
		}
		for _, item := range res.Items {
			lease := new(Lease)
			if err := dynamodbattribute.UnmarshalMap(item, lease); err == nil {
				list = append(list, lease)
				lease.lastRenewal = time.Now()
			}
		}
		break
	}
	l.Backoff.Reset()
	return
}

// Delete the given lease from DynamoDB. does nothing when passed a
// lease that does not exist in DynamoDB.
func (l *LeaseManager) DeleteLease(lease *Lease) (err error) {
	for l.Backoff.Attempt() < maxDeleteRetries {
		_, err = l.Client.DeleteItem(&dynamodb.DeleteItemInput{
			TableName: aws.String(l.LeaseTable),
			Key: map[string]*dynamodb.AttributeValue{
				LeaseKeyKey: {
					S: aws.String(lease.Key),
				},
			},
			ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
				":condOwner": {
					S: aws.String(lease.Owner),
				},
				":condCounter": {
					N: aws.String(strconv.Itoa(lease.Counter)),
				},
			},
			ExpressionAttributeNames: map[string]*string{
				"#counter": aws.String(LeaseCounterKey),
				"#owner":   aws.String(LeaseOwnerKey),
				"#key":     aws.String(LeaseKeyKey),
			},
			ConditionExpression: aws.String("attribute_not_exists(#key) OR :condCounter = #counter AND :condOwner = #owner"),
		})

		if err == nil {
			break
		}

		backoff := l.Backoff.Duration()

		l.Logger.WithFields(logrus.Fields{
			"backoff": backoff,
			"attempt": int(l.Backoff.Attempt()),
		}).Warnf("Worker %s failed to delete lease", l.WorkerId)

		time.Sleep(backoff)
	}
	l.Backoff.Reset()
	return
}

func (l *LeaseManager) CreateLease(lease *Lease) (err error) {
	_, err = l.Client.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String(l.LeaseTable),
		Item: map[string]*dynamodb.AttributeValue{
			LeaseKeyKey: {
				S: aws.String(lease.Key),
			},
		},
		/*ExpressionAttributeNames: map[string]*string{
			"#key": aws.String(LeaseKeyKey),
		},
		ConditionExpression: aws.String("attribute_not_exists(#key)"),*/
	})
	l.Logger.Info(err)
	return err
}
